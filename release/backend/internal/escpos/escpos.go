package escpos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"

	"sirbeef/backend/internal/models"
)

// ExtractOrderProminentModality retorna o título GIGANTE da modalidade/mesa (ex: "MESA 15", "RETIRADA", "BALCAO", "ENTREGA")
// nunca usando a abreviação "M15", para que fique super visível para a equipe na impressão térmica!
func ExtractOrderProminentModality(order models.Order) (bigBanner string, detailLine string) {
	orig := strings.ToUpper(strings.TrimSpace(order.OriginType))
	hdr := strings.TrimSpace(order.HeaderTag)
	cmdCode := strings.TrimSpace(order.ComandaCode)
	addr := strings.TrimSpace(order.AddressText)

	// Substitui qualquer prefixo abreviado "M15" / "M01" por "MESA 15" / "MESA 01"
	expandMCode := func(s string) string {
		sTrim := strings.TrimSpace(s)
		if len(sTrim) >= 2 && (sTrim[0] == 'M' || sTrim[0] == 'm') && sTrim[1] >= '0' && sTrim[1] <= '9' {
			j := 1
			for j < len(sTrim) && sTrim[j] >= '0' && sTrim[j] <= '9' {
				j++
			}
			numPart := sTrim[1:j]
			rest := strings.TrimSpace(sTrim[j:])
			if rest != "" {
				return fmt.Sprintf("MESA %s - %s", numPart, strings.TrimLeft(rest, "-• "))
			}
			return fmt.Sprintf("MESA %s", numPart)
		}
		return sTrim
	}

	hdr = expandMCode(hdr)
	cmdCode = expandMCode(cmdCode)

	extractMesaFromText := func(txt string) string {
		up := strings.ToUpper(txt)
		idx := strings.Index(up, "MESA")
		if idx >= 0 {
			after := strings.TrimSpace(up[idx+4:])
			j := 0
			for j < len(after) && after[j] >= '0' && after[j] <= '9' {
				j++
			}
			if j > 0 {
				return fmt.Sprintf("MESA %s", after[:j])
			}
		}
		return ""
	}

	if orig == "AUTODETECT" || strings.Contains(strings.ToUpper(hdr), "TESTE") {
		return "TESTE IMPRESSORA", hdr
	}

	isMesaOrder := orig == "MESA" || orig == "TABLET" || orig == "GARCOM" || (order.TableID != nil && *order.TableID > 0) || strings.Contains(strings.ToUpper(hdr), "MESA") || strings.Contains(strings.ToUpper(addr), "MESA")
	if isMesaOrder {
		mesaTitle := extractMesaFromText(addr)
		if mesaTitle == "" {
			mesaTitle = extractMesaFromText(hdr)
		}
		if mesaTitle == "" {
			mesaTitle = extractMesaFromText(cmdCode)
		}
		if mesaTitle == "" && order.TableID != nil && *order.TableID > 0 {
			mesaTitle = fmt.Sprintf("MESA %02d", *order.TableID)
		}
		if mesaTitle == "" {
			mesaTitle = "MESA"
		}
		cName := strings.TrimSpace(order.CustomerName)
		if cName != "" && !strings.EqualFold(cName, mesaTitle) {
			return mesaTitle, fmt.Sprintf("%s • %s", mesaTitle, strings.ToUpper(cName))
		}
		return mesaTitle, hdr
	}

	if orig == "RETIRADA" || strings.Contains(strings.ToUpper(hdr), "RETIRADA") {
		cName := strings.TrimSpace(order.CustomerName)
		if cName != "" {
			return "RETIRADA", fmt.Sprintf("RETIRADA NO BALCAO • %s", strings.ToUpper(cName))
		}
		return "RETIRADA", "PEDIDO PARA RETIRADA NO BALCAO"
	}

	if orig == "DELIVERY" || strings.Contains(strings.ToUpper(hdr), "DELIVERY") || strings.Contains(strings.ToUpper(hdr), "ENTREGA") {
		cName := strings.TrimSpace(order.CustomerName)
		if cName != "" {
			return "ENTREGA", fmt.Sprintf("ENTREGA DELIVERY • %s", strings.ToUpper(cName))
		}
		return "ENTREGA", "PEDIDO PARA ENTREGA (DELIVERY)"
	}

	cName := strings.TrimSpace(order.CustomerName)
	if cName != "" && !strings.EqualFold(cName, "Consumidor Balcão") && !strings.EqualFold(cName, "Consumidor Balcao") {
		return "BALCAO", fmt.Sprintf("BALCAO • %s", strings.ToUpper(cName))
	}
	return "BALCAO", "VENDA DIRETA NO BALCAO"
}

// writeTicketTextWithGiantHeaders escreve o texto do cupom convertendo as linhas de destaque (>>> [ ... ] <<<)
// em fonte GIGANTE (Dupla Altura + Dupla Largura GS ! 0x11 + Negrito ESC E 1) nas impressoras térmicas ESC/POS!
func writeTicketTextWithGiantHeaders(buf *bytes.Buffer, ticketText string) {
	lines := strings.Split(strings.ReplaceAll(ticketText, "\r\n", "\n"), "\n")
	for _, line := range lines {
		trimLine := strings.TrimSpace(line)
		if strings.HasPrefix(trimLine, ">>>") && strings.HasSuffix(trimLine, "<<<") && strings.Contains(trimLine, "[") && strings.Contains(trimLine, "]") {
			openIdx := strings.Index(trimLine, "[")
			closeIdx := strings.LastIndex(trimLine, "]")
			if openIdx >= 0 && closeIdx > openIdx {
				giantText := strings.TrimSpace(trimLine[openIdx+1 : closeIdx])
				// Centraliza (ESC a 1), Liga Negrito (ESC E 1), Liga Dupla Largura + Dupla Altura (GS ! 0x11)
				buf.Write([]byte{0x1B, 0x61, 0x01, 0x1B, 0x45, 0x01, 0x1D, 0x21, 0x11})
				buf.WriteString(giantText + "\n")
				// Desliga tamanho duplo (GS ! 0x00), Desliga Negrito (ESC E 0), Alinha à esquerda (ESC a 0)
				buf.Write([]byte{0x1D, 0x21, 0x00, 0x1B, 0x45, 0x00, 0x1B, 0x61, 0x00})
				continue
			}
		}
		// Destaca itens (ex: "1x PICANHA") e linha de CLIENTE em Negrito para leitura fácil na cozinha e no caixa
		isItemOrClient := (len(trimLine) >= 3 && trimLine[0] >= '1' && trimLine[0] <= '9' && strings.Contains(trimLine[:4], "x ")) ||
			strings.HasPrefix(trimLine, "CLIENTE:") ||
			strings.HasPrefix(trimLine, "TOTAL FINAL:") ||
			strings.HasPrefix(trimLine, "TOTAL DA CONTA:")
		if isItemOrClient {
			buf.Write([]byte{0x1B, 0x45, 0x01})
			buf.WriteString(line + "\n")
			buf.Write([]byte{0x1B, 0x45, 0x00})
		} else {
			buf.WriteString(line + "\n")
		}
	}
}

// RenderTextReceipt gera o cupom térmico dinâmico adaptando quebras de linha e colunas
// exatamente para bobinas de 58mm (32 colunas) ou 80mm (48 colunas).
func RenderTextReceipt(order models.Order, columns int) string {
	if columns != 32 && columns != 48 {
		columns = 48
	}

	var sb strings.Builder
	dividerEq := strings.Repeat("=", columns)
	dividerStar := strings.Repeat("*", columns)
	dividerDash := strings.Repeat("-", columns)

	sb.WriteString(dividerEq + "\n")
	sb.WriteString(centerText("SIR BEEF - BOUTIQUE DAS CARNES", columns) + "\n")
	sb.WriteString(centerText("PDV & LOGISTICA INTEGRADA", columns) + "\n")
	sb.WriteString(dividerStar + "\n")

	bigBanner, detailLine := ExtractOrderProminentModality(order)
	sb.WriteString(centerText(">>> [ "+bigBanner+" ] <<<", columns) + "\n")
	if detailLine != "" && !strings.EqualFold(detailLine, bigBanner) {
		sb.WriteString(centerText(detailLine, columns) + "\n")
	}
	sb.WriteString(dividerStar + "\n")
	sb.WriteString(formatLeftRight(fmt.Sprintf("PEDIDO: #%s", order.OrderCode), order.CreatedAt, columns) + "\n")
	sb.WriteString(dividerDash + "\n")

	if order.CustomerName != "" {
		for _, line := range wrapText("CLIENTE: "+strings.ToUpper(order.CustomerName), columns) {
			sb.WriteString(line + "\n")
		}
	}
	if order.CustomerPhone != "" {
		sb.WriteString(formatLeftRight("WHATSAPP:", order.CustomerPhone, columns) + "\n")
	}
	if order.OriginType == "DELIVERY" && order.AddressText != "" {
		for _, line := range wrapText("END: "+order.AddressText, columns) {
			sb.WriteString(line + "\n")
		}
		if order.Latitude != 0 || order.Longitude != 0 {
			sb.WriteString(fmt.Sprintf("GPS: %.5f, %.5f\n", order.Latitude, order.Longitude))
		}
	}
	sb.WriteString(dividerDash + "\n")
	sb.WriteString(formatLeftRight("QTD  ITEM", "TOTAL", columns) + "\n")
	sb.WriteString(dividerDash + "\n")

	for _, item := range order.Items {
		qtyStr := fmt.Sprintf("%.0fx", item.Quantity)
		if item.Quantity != float64(int(item.Quantity)) {
			qtyStr = fmt.Sprintf("%.3fkg", item.Quantity)
		}
		priceStr := fmt.Sprintf("R$ %.2f", item.TotalPrice)
		leftLabel := fmt.Sprintf("%s %s", qtyStr, strings.ToUpper(item.ProductName))
		for _, line := range wrapLeftRight(leftLabel, priceStr, columns) {
			sb.WriteString(line + "\n")
		}

		// Adicionais, Ponto da Carne e Retiradas destacados para a chapa
		for _, mod := range item.SelectedModifiers {
			prefix := "  + "
			if strings.Contains(strings.ToLower(mod.GroupName), "retirada") || strings.HasPrefix(strings.ToLower(mod.OptionName), "sem ") {
				prefix = "  [!] "
			} else if strings.Contains(strings.ToLower(mod.GroupName), "ponto") {
				prefix = "  [*] "
			}
			modLine := prefix + mod.OptionName
			if mod.Price > 0 {
				modPrice := fmt.Sprintf("+R$%.2f", mod.Price)
				sb.WriteString(formatLeftRight(modLine, modPrice, columns) + "\n")
			} else {
				for _, wl := range wrapText(modLine, columns) {
					sb.WriteString(wl + "\n")
				}
			}
		}

		if strings.TrimSpace(item.Notes) != "" {
			for _, wl := range wrapText("  OBS: "+strings.ToUpper(item.Notes), columns) {
				sb.WriteString(wl + "\n")
			}
		}
	}

	sb.WriteString(dividerDash + "\n")
	sb.WriteString(formatLeftRight("SUBTOTAL:", fmt.Sprintf("R$ %.2f", order.Subtotal), columns) + "\n")
	if order.DeliveryFee > 0 {
		sb.WriteString(formatLeftRight("TAXA ENTREGA:", fmt.Sprintf("R$ %.2f", order.DeliveryFee), columns) + "\n")
	}
	if order.DiscountAmount > 0 {
		cupLabel := "DESCONTO CUPOM:"
		if strings.TrimSpace(order.CouponCode) != "" {
			cupLabel = fmt.Sprintf("CUPOM (%s):", strings.ToUpper(strings.TrimSpace(order.CouponCode)))
		}
		sb.WriteString(formatLeftRight(cupLabel, fmt.Sprintf("- R$ %.2f", order.DiscountAmount), columns) + "\n")
	}
	sb.WriteString(formatLeftRight("TOTAL FINAL:", fmt.Sprintf("R$ %.2f", order.Total), columns) + "\n")
	sb.WriteString(dividerDash + "\n")
	sb.WriteString(formatLeftRight("PAGAMENTO:", order.PaymentMethod, columns) + "\n")
	sb.WriteString(formatLeftRight("STATUS PGTO:", order.StatusPagamento, columns) + "\n")
	if order.ChangeFor > order.Total {
		troco := order.ChangeFor - order.Total
		sb.WriteString(formatLeftRight(fmt.Sprintf("TROCO P/ R$%.2f:", order.ChangeFor), fmt.Sprintf("R$ %.2f", troco), columns) + "\n")
	}

	// QR Code de Despacho Logístico (SOMENTE para pedidos de ENTREGA / DELIVERY!)
	sb.WriteString(dividerEq + "\n")
	if strings.EqualFold(order.OriginType, "DELIVERY") && strings.TrimSpace(order.QRPayload) != "" {
		sb.WriteString(centerText("QR CODE DE DESPACHO MOTOBOY", columns) + "\n")
		sb.WriteString(centerText("["+order.QRPayload+"]", columns) + "\n")
		sb.WriteString(centerText("Escaneie no Super App Sir Beef", columns) + "\n")
	} else {
		sb.WriteString(centerText("CONFERENCIA / CONSUMO LOCAL", columns) + "\n")
		sb.WriteString(centerText("OBRIGADO PELA PREFERENCIA!", columns) + "\n")
	}
	sb.WriteString(dividerEq + "\n")
	return sb.String()
}

// RenderSectorTicket gera o ticket de produção exclusivo de uma impressora/setor (ex: só Bebidas no Bar, só Lanches/Porções na Chapa).
func RenderSectorTicket(order models.Order, station models.PrinterStation, items []models.OrderItem) string {
	cols := station.Columns
	if cols != 32 && cols != 48 {
		cols = 48
	}
	var sb strings.Builder
	dividerEq := strings.Repeat("=", cols)
	dividerStar := strings.Repeat("*", cols)
	dividerDash := strings.Repeat("-", cols)

	sb.WriteString(dividerEq + "\n")
	sb.WriteString(centerText("SETOR: "+strings.ToUpper(station.Name), cols) + "\n")
	sb.WriteString(dividerStar + "\n")
	bigBanner, detailLine := ExtractOrderProminentModality(order)
	sb.WriteString(centerText(">>> [ "+bigBanner+" ] <<<", cols) + "\n")
	if detailLine != "" && !strings.EqualFold(detailLine, bigBanner) {
		sb.WriteString(centerText(detailLine, cols) + "\n")
	}
	sb.WriteString(dividerStar + "\n")
	sb.WriteString(formatLeftRight(fmt.Sprintf("PEDIDO: #%s", order.OrderCode), order.CreatedAt, cols) + "\n")
	if order.CustomerName != "" {
		sb.WriteString(truncateRunes("CLIENTE: "+strings.ToUpper(order.CustomerName), cols) + "\n")
	}
	sb.WriteString(dividerDash + "\n")

	for _, item := range items {
		qtyStr := fmt.Sprintf("%.0fx", item.Quantity)
		if item.Quantity != float64(int(item.Quantity)) {
			qtyStr = fmt.Sprintf("%.3fkg", item.Quantity)
		}
		line := fmt.Sprintf("%s %s", qtyStr, strings.ToUpper(item.ProductName))
		for _, wl := range wrapText(line, cols) {
			sb.WriteString(wl + "\n")
		}
		for _, mod := range item.SelectedModifiers {
			prefix := "  + "
			if strings.Contains(strings.ToLower(mod.GroupName), "retirada") || strings.HasPrefix(strings.ToLower(mod.OptionName), "sem ") {
				prefix = "  [!] "
			} else if strings.Contains(strings.ToLower(mod.GroupName), "ponto") {
				prefix = "  [*] "
			}
			for _, wl := range wrapText(prefix+mod.OptionName, cols) {
				sb.WriteString(wl + "\n")
			}
		}
		if strings.TrimSpace(item.Notes) != "" {
			for _, wl := range wrapText("  OBS: "+strings.ToUpper(item.Notes), cols) {
				sb.WriteString(wl + "\n")
			}
		}
		sb.WriteString(dividerDash + "\n")
	}
	targetInfo := station.WindowsPrinterName
	if strings.TrimSpace(targetInfo) == "" {
		targetInfo = fmt.Sprintf("%s:%d", station.PrinterIP, station.PrinterPort)
	}
	if station.PCName != "" {
		targetInfo = fmt.Sprintf("[%s] %s", station.PCName, targetInfo)
	}
	sb.WriteString(centerText(fmt.Sprintf("DESTINO: %s (%s)", station.ID, targetInfo), cols) + "\n")
	if station.CutPaperSeparately {
		sb.WriteString(centerText("--- [CORTE AUTOMATICO GUILHOTINA] ---", cols) + "\n")
	}
	sb.WriteString(dividerEq + "\n")
	return sb.String()
}

// RenderWaiterCallTicket gera o ticket térmico imediato para a Impressora do Caixa quando uma mesa chama o garçom ou pede a conta.
func RenderWaiterCallTicket(tableLabel string, reason string, comandaCode string, customerName string, partialTotal float64, columns int) string {
	if columns != 32 && columns != 48 {
		columns = 48
	}
	var sb strings.Builder
	divEq := strings.Repeat("=", columns)
	divStar := strings.Repeat("*", columns)
	divDash := strings.Repeat("-", columns)

	cleanTable := strings.ToUpper(strings.TrimSpace(tableLabel))
	if cleanTable == "" {
		cleanTable = "MESA"
	}

	sb.WriteString(divEq + "\n")
	sb.WriteString(centerText("SIR BEEF - ALERTA DE ATENDIMENTO", columns) + "\n")
	sb.WriteString(divStar + "\n")
	sb.WriteString(centerText(">>> [ "+cleanTable+" ] <<<", columns) + "\n")
	sb.WriteString(centerText(strings.ToUpper(reason), columns) + "\n")
	sb.WriteString(divStar + "\n")
	sb.WriteString(formatLeftRight("MESA / LOCAL:", cleanTable, columns) + "\n")
	sb.WriteString(formatLeftRight("HORARIO:", time.Now().Format("02/01 15:04:05"), columns) + "\n")
	if strings.TrimSpace(comandaCode) != "" {
		sb.WriteString(formatLeftRight("COMANDA:", comandaCode, columns) + "\n")
	}
	if strings.TrimSpace(customerName) != "" {
		sb.WriteString(formatLeftRight("CLIENTE:", strings.ToUpper(customerName), columns) + "\n")
	}
	if partialTotal > 0 {
		sb.WriteString(formatLeftRight("PARCIAL CONTA:", fmt.Sprintf("R$ %.2f", partialTotal), columns) + "\n")
	}
	sb.WriteString(divDash + "\n")
	sb.WriteString(centerText("ACIONAR GARCOM IMEDIATAMENTE!", columns) + "\n")
	sb.WriteString(divEq + "\n")
	return sb.String()
}

// RenderComandaClosureTicket gera o cupom de fechamento/conferência de conta para a Impressora do Caixa.
func RenderComandaClosureTicket(cmd models.Comanda, columns int) string {
	if columns != 32 && columns != 48 {
		columns = 48
	}
	var sb strings.Builder
	divEq := strings.Repeat("=", columns)
	divStar := strings.Repeat("*", columns)
	divDash := strings.Repeat("-", columns)

	mesaTitle := strings.ToUpper(strings.TrimSpace(cmd.TableLabel))
	if mesaTitle == "" && cmd.TableNumber > 0 {
		mesaTitle = fmt.Sprintf("MESA %02d", cmd.TableNumber)
	}
	if mesaTitle == "" {
		mesaTitle = "CONTA MESA"
	}

	sb.WriteString(divEq + "\n")
	sb.WriteString(centerText("SIR BEEF - BOUTIQUE DAS CARNES", columns) + "\n")
	sb.WriteString(centerText("FECHAMENTO / CONFERENCIA DE CONTA", columns) + "\n")
	sb.WriteString(divStar + "\n")
	sb.WriteString(centerText(">>> [ "+mesaTitle+" ] <<<", columns) + "\n")
	sb.WriteString(divStar + "\n")
	sb.WriteString(formatLeftRight("MESA: "+mesaTitle, "COMANDA: "+cmd.ComandaCode, columns) + "\n")
	if strings.TrimSpace(cmd.CustomerName) != "" {
		sb.WriteString(formatLeftRight("CLIENTE:", strings.ToUpper(cmd.CustomerName), columns) + "\n")
	}
	sb.WriteString(formatLeftRight("ABERTURA: "+cmd.OpenedAt, "AGORA: "+time.Now().Format("15:04"), columns) + "\n")
	sb.WriteString(divDash + "\n")
	sb.WriteString(formatLeftRight("QTD  ITEM", "TOTAL", columns) + "\n")
	sb.WriteString(divDash + "\n")
	for _, it := range cmd.Items {
		qStr := fmt.Sprintf("%.0fx", it.Quantity)
		pStr := fmt.Sprintf("R$ %.2f", it.TotalPrice)
		for _, ln := range wrapLeftRight(qStr+" "+strings.ToUpper(it.ProductName), pStr, columns) {
			sb.WriteString(ln + "\n")
		}
		for _, m := range it.SelectedModifiers {
			sb.WriteString("  + " + m.OptionName + "\n")
		}
	}
	sb.WriteString(divDash + "\n")
	sb.WriteString(formatLeftRight("TOTAL DA CONTA:", fmt.Sprintf("R$ %.2f", cmd.Total), columns) + "\n")
	if strings.TrimSpace(cmd.PaymentMethod) != "" {
		sb.WriteString(formatLeftRight("FORMA PGTO:", cmd.PaymentMethod, columns) + "\n")
	}
	sb.WriteString(formatLeftRight("STATUS:", cmd.Status, columns) + "\n")
	sb.WriteString(divEq + "\n")
	sb.WriteString(centerText("OBRIGADO PELA PREFERENCIA!", columns) + "\n")
	sb.WriteString(divEq + "\n")
	return sb.String()
}

// BuildRawTextTicketESCPOS transforma qualquer texto de setor/conta/alerta em payload ESC/POS binário com comando de corte de papel (guilhotina).
func BuildRawTextTicketESCPOS(ticketText string, cutPaper bool) []byte {
	var buf bytes.Buffer
	// ESC @ - Inicializa impressora
	buf.Write([]byte{0x1B, 0x40})
	// Seleciona página de código PC850 / Latin
	buf.Write([]byte{0x1B, 0x74, 0x02})
	writeTicketTextWithGiantHeaders(&buf, ticketText)
	buf.Write([]byte("\n\n\n\n"))
	if cutPaper {
		// GS V 66 0 - Aciona guilhotina (corte parcial/total compatível com todas as térmicas Epson/Elgin/Bematech/POS-80)
		buf.Write([]byte{0x1D, 0x56, 0x42, 0x00})
	}
	return buf.Bytes()
}

// BuildRawESCPOS constrói o payload binário ESC/POS universal incluindo QR Code nativo (GS ( k) apenas para DELIVERY e corte de papel.
func BuildRawESCPOS(order models.Order, columns int) []byte {
	var buf bytes.Buffer

	// ESC @ - Inicializa impressora
	buf.Write([]byte{0x1B, 0x40})
	// Seleciona página de código PC850 / Latin
	buf.Write([]byte{0x1B, 0x74, 0x02})

	// Texto formatado dinamicamente para 32 ou 48 colunas com cabeçalho GIGANTE (MESA 15 / RETIRADA / BALCAO / ENTREGA)
	textReceipt := RenderTextReceipt(order, columns)
	writeTicketTextWithGiantHeaders(&buf, textReceipt)

	// Imprime o QR Code físico na bobina térmica SOMENTE para pedidos de ENTREGA (DELIVERY)
	if strings.EqualFold(order.OriginType, "DELIVERY") && strings.TrimSpace(order.QRPayload) != "" {
		buf.Write([]byte{0x1B, 0x61, 0x01})
		qrData := []byte(order.QRPayload)
		if len(qrData) > 0 {
			// Modelo 2 do QR Code: GS ( k 04 00 31 41 32 00
			buf.Write([]byte{0x1D, 0x28, 0x6B, 0x04, 0x00, 0x31, 0x41, 0x32, 0x00})
			// Tamanho do módulo (6): GS ( k 03 00 31 43 06
			buf.Write([]byte{0x1D, 0x28, 0x6B, 0x03, 0x00, 0x31, 0x43, 0x06})
			// Nível de correção de erro M (49): GS ( k 03 00 31 45 31
			buf.Write([]byte{0x1D, 0x28, 0x6B, 0x03, 0x00, 0x31, 0x45, 0x31})
			// Armazena dados no buffer do QR Code: GS ( k pL pH 31 50 30 <data>
			storeLen := len(qrData) + 3
			pL := byte(storeLen & 0xFF)
			pH := byte((storeLen >> 8) & 0xFF)
			buf.Write([]byte{0x1D, 0x28, 0x6B, pL, pH, 0x31, 0x50, 0x30})
			buf.Write(qrData)
			// Imprime o QR Code armazenado: GS ( k 03 00 31 51 30
			buf.Write([]byte{0x1D, 0x28, 0x6B, 0x03, 0x00, 0x31, 0x51, 0x30})
		}
	}

	// Avança 4 linhas e aciona guilhotina parcial (GS V 66 0)
	buf.Write([]byte("\n\n\n\n"))
	buf.Write([]byte{0x1D, 0x56, 0x42, 0x00})

	return buf.Bytes()
}

// SendToNetworkPrinter envia os bytes ESC/POS via TCP (porta 9100 padrão de impressoras térmicas Ethernet/Wi-Fi).
func SendToNetworkPrinter(printerIP string, port int, payload []byte) error {
	if strings.TrimSpace(printerIP) == "" {
		return nil // Modo simulado/preview local quando sem IP físico configurado
	}
	if port <= 0 {
		port = 9100
	}
	addr := fmt.Sprintf("%s:%d", printerIP, port)
	conn, err := net.DialTimeout("tcp", addr, 2500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("falha ao conectar na impressora térmica %s: %w", addr, err)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = conn.Write(payload)
	return err
}

type docInfo1W struct {
	pDocName    *uint16
	pOutputFile *uint16
	pDatatype   *uint16
}

// SendRawToWindowsSpooler envia bytes ESC/POS RAW diretamente para qualquer impressora instalada no Windows
// (USB, Bluetooth ou Rede Compartilhada) usando a API nativa winspool.drv, incluindo comando de corte da guilhotina!
func SendRawToWindowsSpooler(printerName string, payload []byte) error {
	pName := strings.TrimSpace(printerName)
	if pName == "" || len(payload) == 0 {
		return nil
	}
	winspool := syscall.NewLazyDLL("winspool.drv")
	openPrinter := winspool.NewProc("OpenPrinterW")
	startDocPrinter := winspool.NewProc("StartDocPrinterW")
	startPagePrinter := winspool.NewProc("StartPagePrinter")
	writePrinter := winspool.NewProc("WritePrinter")
	endPagePrinter := winspool.NewProc("EndPagePrinter")
	endDocPrinter := winspool.NewProc("EndDocPrinter")
	closePrinter := winspool.NewProc("ClosePrinter")

	pNameUTF16, err := syscall.UTF16PtrFromString(pName)
	if err != nil {
		return err
	}
	var hPrinter uintptr
	r1, _, lastErr := openPrinter.Call(uintptr(unsafe.Pointer(pNameUTF16)), uintptr(unsafe.Pointer(&hPrinter)), 0)
	if r1 == 0 || hPrinter == 0 {
		return fmt.Errorf("impressora Windows '%s' não encontrada ou inacessível: %v", pName, lastErr)
	}
	defer closePrinter.Call(hPrinter)

	docName, _ := syscall.UTF16PtrFromString("SirBeef ESC/POS Ticket")
	dataType, _ := syscall.UTF16PtrFromString("RAW")
	di := docInfo1W{
		pDocName:    docName,
		pOutputFile: nil,
		pDatatype:   dataType,
	}
	rDoc, _, lastErr := startDocPrinter.Call(hPrinter, 1, uintptr(unsafe.Pointer(&di)))
	if rDoc == 0 {
		return fmt.Errorf("falha em StartDocPrinter na impressora '%s': %v", pName, lastErr)
	}
	defer endDocPrinter.Call(hPrinter)

	rPage, _, _ := startPagePrinter.Call(hPrinter)
	if rPage != 0 {
		defer endPagePrinter.Call(hPrinter)
	}

	var written uint32
	rWrite, _, lastErr := writePrinter.Call(
		hPrinter,
		uintptr(unsafe.Pointer(&payload[0])),
		uintptr(uint32(len(payload))),
		uintptr(unsafe.Pointer(&written)),
	)
	if rWrite == 0 {
		return fmt.Errorf("falha ao escrever bytes RAW na impressora '%s': %v", pName, lastErr)
	}
	return nil
}

// ClassifyPrinterConnection identifica automaticamente se a impressora do Windows é USB, Bluetooth ou Rede Compartilhada.
func ClassifyPrinterConnection(printerName, portName, driverName string, shared bool) (connType string, badge string) {
	combined := strings.ToUpper(printerName + " " + portName + " " + driverName)
	portUp := strings.ToUpper(strings.TrimSpace(portName))
	if strings.Contains(combined, "BTH") || strings.Contains(combined, "BLUETOOTH") || strings.HasPrefix(portUp, "COM") || strings.Contains(combined, "BT-") || strings.Contains(combined, "BLUE") {
		return "BLUETOOTH", "📶 Bluetooth"
	}
	if strings.HasPrefix(portUp, "\\\\") || strings.Contains(portUp, "IP_") || strings.Contains(portUp, "WSD") || strings.Contains(portUp, "TCP") || strings.Contains(portUp, "192.") || strings.Contains(portUp, "10.") || shared {
		return "REDE_COMPARTILHADA", "🌐 Rede Compartilhada"
	}
	return "USB_RAW", "🔌 USB"
}

// DetectLocalWindowsPrinters autodetecta todas as impressoras instaladas no Windows deste PC (USB, Bluetooth e Rede Compartilhada).
func DetectLocalWindowsPrinters(pcName string, pcRoleLabel string) []models.DetectedPrinter {
	if strings.TrimSpace(pcName) == "" {
		if hn, err := os.Hostname(); err == nil && strings.TrimSpace(hn) != "" {
			pcName = strings.ToUpper(strings.TrimSpace(hn))
		} else {
			pcName = "PC_COZINHA"
		}
	}
	if strings.TrimSpace(pcRoleLabel) == "" {
		pcRoleLabel = fmt.Sprintf("🖥️ %s (Windows Local)", pcName)
	}

	psScript := `Get-CimInstance Win32_Printer | Select-Object Name, PortName, DriverName, Shared, ShareName, WorkOffline | ConvertTo-Json -Compress`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	out, err := cmd.Output()
	nowStr := time.Now().Format("15:04:05")
	var list []models.DetectedPrinter

	if err == nil && len(out) > 0 {
		rawStr := strings.TrimSpace(string(out))
		type winPrn struct {
			Name        string `json:"Name"`
			PortName    string `json:"PortName"`
			DriverName  string `json:"DriverName"`
			Shared      bool   `json:"Shared"`
			ShareName   string `json:"ShareName"`
			WorkOffline bool   `json:"WorkOffline"`
		}
		var items []winPrn
		if strings.HasPrefix(rawStr, "[") {
			_ = json.Unmarshal([]byte(rawStr), &items)
		} else if strings.HasPrefix(rawStr, "{") {
			var single winPrn
			if json.Unmarshal([]byte(rawStr), &single) == nil && single.Name != "" {
				items = append(items, single)
			}
		}
		for _, it := range items {
			nameTrim := strings.TrimSpace(it.Name)
			if nameTrim == "" {
				continue
			}
			// Filtra impressoras virtuais padrão do Windows que não são impressoras físicas, mantendo qualquer térmica/física
			nameLow := strings.ToLower(nameTrim)
			if strings.Contains(nameLow, "onenote") || strings.Contains(nameLow, "xps document") || strings.Contains(nameLow, "fax") {
				continue
			}
			cType, badge := ClassifyPrinterConnection(nameTrim, it.PortName, it.DriverName, it.Shared)
			st := "ONLINE"
			if it.WorkOffline {
				st = "OFFLINE"
			}
			list = append(list, models.DetectedPrinter{
				ID:                 fmt.Sprintf("%s::%s", pcName, nameTrim),
				Key:                fmt.Sprintf("%s|%s", pcName, nameTrim),
				Name:               nameTrim,
				PrinterName:        nameTrim,
				PCName:             pcName,
				PCRoleLabel:        pcRoleLabel,
				WindowsPrinterName: nameTrim,
				PortName:           strings.TrimSpace(it.PortName),
				DriverName:         strings.TrimSpace(it.DriverName),
				ConnectionType:     cType,
				ConnectionBadge:    badge,
				Shared:             it.Shared,
				ShareName:          strings.TrimSpace(it.ShareName),
				Status:             st,
				LastSeenAt:         nowStr,
			})
		}
	}

	// Garante sempre perfis de referência prontos dos 2 PCs (PC Cozinha USB + PC Caixa Rede + PC Caixa Bluetooth) caso ainda não estejam plugadas no PC de desenvolvimento
	hasKitchenUSB := false
	for _, p := range list {
		if p.ConnectionType == "USB_RAW" || p.ConnectionType == "USB" {
			hasKitchenUSB = true
		}
	}
	if !hasKitchenUSB {
		list = append(list, models.DetectedPrinter{
			ID:                 "PC_COZINHA::Impressora Termica Cozinha USB (Win11)",
			Key:                "PC_COZINHA|Impressora Termica Cozinha USB (Win11)",
			Name:               "Impressora Termica Cozinha USB (Win11)",
			PrinterName:        "Impressora Termica Cozinha USB (Win11)",
			PCName:             "PC_COZINHA",
			PCRoleLabel:        "🖥️ PC Cozinha (Windows 11)",
			WindowsPrinterName: "Impressora Termica Cozinha USB (Win11)",
			PortName:           "USB001",
			DriverName:         "ESC/POS Thermal USB 80mm",
			ConnectionType:     "USB_RAW",
			ConnectionBadge:    "🔌 USB (Cozinha Win11)",
			Shared:             false,
			Status:             "PRONTA P/ VINCULAR",
			LastSeenAt:         nowStr,
		})
	}
	return list
}

func centerText(s string, width int) string {
	s = strings.TrimSpace(s)
	runeLen := utf8.RuneCountInString(s)
	if runeLen >= width {
		return truncateRunes(s, width)
	}
	leftPad := (width - runeLen) / 2
	return strings.Repeat(" ", leftPad) + s
}

func formatLeftRight(left, right string, width int) string {
	lLen := utf8.RuneCountInString(left)
	rLen := utf8.RuneCountInString(right)
	if lLen+rLen+1 > width {
		maxLeft := width - rLen - 1
		if maxLeft < 4 {
			return truncateRunes(left+" "+right, width)
		}
		left = truncateRunes(left, maxLeft)
		lLen = utf8.RuneCountInString(left)
	}
	spaces := width - lLen - rLen
	if spaces < 1 {
		spaces = 1
	}
	return left + strings.Repeat(" ", spaces) + right
}

func wrapLeftRight(left, right string, width int) []string {
	rLen := utf8.RuneCountInString(right)
	firstLineMax := width - rLen - 1
	if firstLineMax < 10 {
		firstLineMax = width
	}
	wrapped := wrapText(left, firstLineMax)
	if len(wrapped) == 0 {
		return []string{formatLeftRight("", right, width)}
	}
	wrapped[0] = formatLeftRight(wrapped[0], right, width)
	return wrapped
}

func wrapText(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	current := words[0]
	for _, w := range words[1:] {
		if utf8.RuneCountInString(current)+1+utf8.RuneCountInString(w) <= width {
			current += " " + w
		} else {
			lines = append(lines, current)
			current = w
		}
	}
	lines = append(lines, current)
	return lines
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
