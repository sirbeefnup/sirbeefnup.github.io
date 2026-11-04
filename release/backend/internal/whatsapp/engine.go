package whatsapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"sirbeef/backend/internal/models"
)

// BuildStaticGreeting gera a saudação automática da Fase 1 (Bot Estático) com o link do cardápio digital.
func BuildStaticGreeting(customerName string, webMenuURL string) string {
	if strings.TrimSpace(customerName) == "" {
		customerName = "Cliente"
	}
	if strings.TrimSpace(webMenuURL) == "" {
		webMenuURL = "http://localhost:8080/cardapio"
	}
	return fmt.Sprintf(
		"🥩 *Olá, %s! Bem-vindo ao SIR BEEF — Boutique das Carnes & Burgers!*\n\n"+
			"Faça seu pedido rapidamente e sem filas pelo nosso Cardápio Digital:\n"+
			"👉 %s\n\n"+
			"Assim que finalizar, você receberá seu comprovante e o status do preparo por aqui automaticamente! 🔥🍔",
		customerName, webMenuURL,
	)
}

// BuildClosedStoreReply gera a mensagem automática do Bot do WhatsApp quando a loja está FECHADA no momento,
// enviando o cronograma completo de Horário de Funcionamento configurado no PDV.
func BuildClosedStoreReply(customerName string, cfg models.PrinterConfig, webMenuURL string) string {
	if strings.TrimSpace(customerName) == "" {
		customerName = "Cliente"
	}
	if strings.TrimSpace(webMenuURL) == "" {
		webMenuURL = "http://localhost:8080/cardapio"
	}

	var scheduleLines []string
	// Ordem amigável: Segunda (1) a Sábado (6) e Domingo (0)
	orderedWeekdays := []int{1, 2, 3, 4, 5, 6, 0}
	dayMap := map[int]models.DayOperatingHours{}
	for _, d := range cfg.OperatingHours.Days {
		wd := d.DayOfWeek
		if wd == 7 {
			wd = 0
		}
		dayMap[wd] = d
	}

	for _, wd := range orderedWeekdays {
		if d, ok := dayMap[wd]; ok {
			lbl := strings.TrimSpace(d.DayName)
			if lbl == "" {
				switch wd {
				case 0:
					lbl = "Domingo"
				case 1:
					lbl = "Segunda-feira"
				case 2:
					lbl = "Terça-feira"
				case 3:
					lbl = "Quarta-feira"
				case 4:
					lbl = "Quinta-feira"
				case 5:
					lbl = "Sexta-feira"
				case 6:
					lbl = "Sábado"
				}
			}
			if d.Enabled || d.IsOpen {
				scheduleLines = append(scheduleLines, fmt.Sprintf("🟢 *%s:* %s às %s", lbl, d.OpenTime, d.CloseTime))
			} else {
				scheduleLines = append(scheduleLines, fmt.Sprintf("🔴 *%s:* Fechado", lbl))
			}
		}
	}

	scheduleText := strings.Join(scheduleLines, "\n")
	if scheduleText == "" {
		scheduleText = "🟢 *Terça a Domingo:* 18:00 às 23:30\n🔴 *Segunda-feira:* Fechado"
	}

	return fmt.Sprintf(
		"🥩 *Olá, %s! Bem-vindo ao SIR BEEF — Boutique das Carnes & Burgers!*\n\n"+
			"🔴 *Estamos FECHADOS no momento.*\n\n"+
			"🕒 *NOSSO HORÁRIO DE FUNCIONAMENTO:*\n%s\n\n"+
			"Você já pode conferir nossos lanches e cortes no Cardápio Digital e fazer seu pedido assim que abrirmos:\n"+
			"👉 %s\n\n"+
			"Esperamos por você! 🔥🍔",
		customerName, scheduleText, webMenuURL,
	)
}

// BuildOrderStatusMessage gera o comprovante/recibo e as notificações automáticas de mudança de status do Kanban.
func BuildOrderStatusMessage(order models.Order) string {
	var sb strings.Builder

	switch order.Status {
	case "RECEBIDO":
		sb.WriteString(fmt.Sprintf("🧾 *COMPROVANTE DE PEDIDO #%s — SIR BEEF*\n", order.OrderCode))
		sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")
		sb.WriteString(fmt.Sprintf("👤 *Cliente:* %s\n", order.CustomerName))
		sb.WriteString(fmt.Sprintf("🏷️ *Origem:* %s\n\n", order.HeaderTag))
		sb.WriteString("*ITENS DO PEDIDO:*\n")
		for _, item := range order.Items {
			sb.WriteString(fmt.Sprintf("• %.0fx *%s* — R$ %.2f\n", item.Quantity, item.ProductName, item.TotalPrice))
			for _, mod := range item.SelectedModifiers {
				sb.WriteString(fmt.Sprintf("   ↳ _%s_\n", mod.OptionName))
			}
		}
		sb.WriteString("━━━━━━━━━━━━━━━━━━━━\n")
		if order.DeliveryFee > 0 {
			sb.WriteString(fmt.Sprintf("🛵 *Taxa de Entrega:* R$ %.2f\n", order.DeliveryFee))
		}
		if order.DiscountAmount > 0 {
			cCode := strings.TrimSpace(order.CouponCode)
			if cCode != "" {
				sb.WriteString(fmt.Sprintf("🎟️ *Desconto Cupom (%s):* - R$ %.2f\n", cCode, order.DiscountAmount))
			} else {
				sb.WriteString(fmt.Sprintf("🎟️ *Desconto Cupom:* - R$ %.2f\n", order.DiscountAmount))
			}
		}
		sb.WriteString(fmt.Sprintf("💰 *TOTAL:* *R$ %.2f*\n", order.Total))
		sb.WriteString(fmt.Sprintf("💳 *Pagamento:* %s (%s)\n", order.PaymentMethod, order.StatusPagamento))
		if order.ChangeFor > order.Total {
			sb.WriteString(fmt.Sprintf("💵 *Troco para R$ %.2f:* R$ %.2f\n", order.ChangeFor, order.ChangeFor-order.Total))
		}
		sb.WriteString("\n✅ *Status:* Recebido no PDV! Já vamos iniciar o preparo.")

	case "PREPARANDO":
		sb.WriteString(fmt.Sprintf("🔥 *SEU PEDIDO #%s ESTÁ NA CHAPA!*\n\n", order.OrderCode))
		sb.WriteString(fmt.Sprintf("Olá *%s*, nossa equipe já está preparando seu pedido no capricho padrão *SIR BEEF*! 🥩🍔\n", order.CustomerName))
		sb.WriteString("Avisaremos você assim que estiver pronto para sair.")

	case "PRONTO":
		sb.WriteString(fmt.Sprintf("🛍️ *PEDIDO #%s PRONTO NA EXPEDIÇÃO!*\n\n", order.OrderCode))
		if order.OriginType == "DELIVERY" {
			sb.WriteString("Seu pedido foi conferido, embalado e já aguarda o bip do nosso entregador.")
		} else {
			sb.WriteString(fmt.Sprintf("Seu pedido (%s) está prontinho!", order.HeaderTag))
		}

	case "DESPACHADO":
		motoboy := order.MotoboyName
		if motoboy == "" {
			motoboy = "Entregador Sir Beef"
		}
		trackURL := strings.TrimSpace(order.TrackingURL)
		if trackURL == "" {
			trackURL = fmt.Sprintf("https://sirbeefnup.github.io/rastreio?pedido=%s", order.OrderCode)
		}
		sb.WriteString(fmt.Sprintf("🛵💨 *SAIU PARA ENTREGA! (PEDIDO #%s)*\n\n", order.OrderCode))
		sb.WriteString(fmt.Sprintf("Nosso entregador *%s* acabou de escanear o QR Code do seu pedido e já está a caminho de:\n📍 _%s_\n\n", motoboy, order.AddressText))
		sb.WriteString(fmt.Sprintf("🗺️ *Acompanhe o entregador em tempo real no mapa:*\n👉 %s\n\n", trackURL))
		if order.StatusPagamento == "PENDENTE" {
			sb.WriteString(fmt.Sprintf("💳 Prepare o pagamento na entrega: *R$ %.2f (%s)*.", order.Total, order.PaymentMethod))
		}

	case "ENTREGUE":
		sb.WriteString(fmt.Sprintf("⭐ *PEDIDO #%s ENTREGUE!*\n\n", order.OrderCode))
		sb.WriteString("Obrigado por escolher o *SIR BEEF — Boutique das Carnes*! Bom apetite! 🥩🔥")
	default:
		sb.WriteString(fmt.Sprintf("ℹ️ *Atualização do Pedido #%s:* Status alterado para *%s*.", order.OrderCode, order.Status))
	}

	return sb.String()
}

// GeneratePhase2GeminiReply implementa o roteamento contextual para a API do Gemini (Fase 2).
func GeneratePhase2GeminiReply(apiKey string, customerName string, incomingText string, menuSummary string, webMenuURL string) (string, error) {
	return GenerateAIOrderBotReply(apiKey, "GEMINI_2_5_FLASH", true, "", customerName, "", 0, incomingText, menuSummary, webMenuURL)
}

// GenerateAIOrderBotReply implementa o atendimento completo por IA (dúvidas + fechamento de pedidos diretos no WhatsApp).
func GenerateAIOrderBotReply(apiKey string, provider string, canCreateOrders bool, customInstructions string, customerName string, savedAddress string, defaultDeliveryFee float64, incomingText string, menuSummary string, webMenuURL string) (string, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return BuildStaticGreeting(customerName, webMenuURL), nil
	}

	addrCtx := "Não cadastrado ainda (pergunte o endereço completo caso seja Entrega Delivery)"
	if strings.TrimSpace(savedAddress) != "" {
		addrCtx = savedAddress
	}

	orderInstruction := ""
	if canCreateOrders {
		orderInstruction = "\nFECHAMENTO AUTOMÁTICO DE PEDIDO:\n" +
			"Se o cliente já escolheu os itens, informou se é ENTREGA (com endereço) ou RETIRADA, e a forma de pagamento (PIX, CARTAO ou DINHEIRO) e confirmou que quer fechar o pedido agora, inclua no FINAL da sua resposta exatamente um bloco JSON no formato abaixo (em uma única linha entre as tags [ORDER_JSON] e [/ORDER_JSON]):\n" +
			"[ORDER_JSON]{\"origin_type\":\"DELIVERY\",\"payment_method\":\"PIX\",\"address_text\":\"Rua X, 123\",\"change_for\":0,\"items\":[{\"product_id\":101,\"product_name\":\"Picanha\",\"quantity\":1,\"unit_price\":36.0,\"notes\":\"Ao Ponto\"}]}[/ORDER_JSON]\n" +
			"Use origin_type=\"BALCAO\" quando for Retirada no Balcão."
	}

	prompt := fmt.Sprintf(
		"Você é o atendente virtual oficial do SIR BEEF — Boutique das Carnes & Hamburgueria Gourmet no WhatsApp.\n"+
			"Atenda de forma simpática, ágil e vendedora.\n"+
			"Regras da loja: %s\n"+
			"Taxa de Entrega Padrão: R$ %.2f\n"+
			"Endereço salvo deste cliente no sistema: %s\n"+
			"Cardápio Oficial atualizado: %s\n"+
			"Link do Cardápio Online (caso o cliente prefira pedir pelo site): %s\n"+
			"%s\n\n"+
			"Cliente (%s) enviou: %s",
		customInstructions, defaultDeliveryFee, addrCtx, menuSummary, webMenuURL, orderInstruction, customerName, incomingText,
	)

	modelName := "gemini-2.5-flash"
	if strings.EqualFold(provider, "GEMINI_2_5_PRO") {
		modelName = "gemini-2.5-pro"
	}

	if strings.EqualFold(provider, "OPENAI_GPT4O_MINI") {
		oaiPayload := map[string]any{
			"model": "gpt-4o-mini",
			"messages": []map[string]string{
				{"role": "user", "content": prompt},
			},
			"temperature": 0.4,
		}
		bodyBytes, _ := json.Marshal(oaiPayload)
		req, err := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(bodyBytes))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+apiKey)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var oaiParsed struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&oaiParsed); err == nil && len(oaiParsed.Choices) > 0 {
			txt := strings.TrimSpace(oaiParsed.Choices[0].Message.Content)
			if txt != "" {
				return txt, nil
			}
		}
		return BuildStaticGreeting(customerName, webMenuURL), nil
	}

	payload := map[string]any{
		"contents": []map[string]any{
			{
				"parts": []map[string]string{
					{"text": prompt},
				},
			},
		},
	}
	bodyBytes, _ := json.Marshal(payload)

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s", modelName, apiKey)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if len(parsed.Candidates) > 0 && len(parsed.Candidates[0].Content.Parts) > 0 {
		return parsed.Candidates[0].Content.Parts[0].Text, nil
	}
	return BuildStaticGreeting(customerName, webMenuURL), nil
}
