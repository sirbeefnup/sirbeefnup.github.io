package models

type User struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Username     string `json:"username"`
	PIN          string `json:"pin"`
	Role         string `json:"role"`         // ADMIN, CAIXA, GARCOM, MOTOBOY, TABLET
	TableNumber  int    `json:"table_number"` // Usado quando Role == TABLET (ex: 1 a 12)
	Active       bool   `json:"active"`       // true = liberado, false = pendente ou bloqueado
	Status       string `json:"status"`       // APROVADO, PENDENTE, BLOQUEADO
	CreatedAt    string `json:"created_at"`
	SessionToken string `json:"session_token"`
}

type ModifierOption struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Price       float64 `json:"price"`
	Visible     *bool   `json:"visible,omitempty"`
	Disabled    bool    `json:"disabled,omitempty"`
}

type ModifierGroup struct {
	ID       int              `json:"id,omitempty"`
	Name     string           `json:"name"`
	Min      int              `json:"min"`
	Max      int              `json:"max"`
	Required bool             `json:"required"`
	Options  []ModifierOption `json:"options"`
}

type WeeklyPromotion struct {
	ID            int     `json:"id"`
	DayOfWeek     int     `json:"day_of_week"` // 0=Domingo, 1=Segunda, 2=Terça, 3=Quarta, 4=Quinta, 5=Sexta, 6=Sábado, 7=Todos os Dias
	DayName       string  `json:"day_name"`
	ProductID     int     `json:"product_id"`
	ProductName   string  `json:"product_name"`
	CategoryName  string  `json:"category_name"`
	OriginalPrice float64 `json:"original_price"`
	PromoPrice    float64 `json:"promo_price"`
	Active        bool    `json:"active"`
	UpdatedAt     string  `json:"updated_at"`
}

type Product struct {
	ID             int             `json:"id"`
	ExternalID     int             `json:"external_id"`
	CategoryID     int             `json:"category_id"`
	CategoryName   string          `json:"category_name"`
	Sector         string          `json:"sector"`     // HAMBURGUERIA, MERCADO
	SectorABC      string          `json:"sector_abc"` // HAMBURGUERIA, MERCADO_CARNES, MERCADO_BEBIDAS
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	Price          float64         `json:"price"`
	PromoFrom      *float64        `json:"promo_from,omitempty"`
	CostPrice      float64         `json:"cost_price"`
	EAN13          string          `json:"ean13"`
	PLUCode        string          `json:"plu_code,omitempty"`
	IsWeighted     bool            `json:"is_weighted"`
	NCM            string          `json:"ncm"`
	CEST           string          `json:"cest"`
	CFOP           string          `json:"cfop"`
	ImageURL       string          `json:"image_url"`
	ModifierGroups []ModifierGroup `json:"modifier_groups"`
	StockQty       float64         `json:"stock_qty"`
	MinStock       float64         `json:"min_stock"`
	TrackStock     bool            `json:"track_stock"`
	Active         bool            `json:"active"`
}

type Category struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Sector    string    `json:"sector"`
	SectorABC string    `json:"sector_abc"`
	Items     []Product `json:"items"`
}

type WeeklyPromoItem struct {
	ProductID     int     `json:"product_id"`
	ProductName   string  `json:"product_name"`
	OriginalPrice float64 `json:"original_price"`
	PromoPrice    float64 `json:"promo_price"`
	Active        bool    `json:"active"`
	ImageURL      string  `json:"image_url"`
	CategoryID    int     `json:"category_id"`
	CategoryName  string  `json:"category_name"`
	Sector        string  `json:"sector"`
}

type WeeklyPromotionsConfig struct {
	Active   bool                         `json:"active"`
	Schedule map[string][]WeeklyPromoItem `json:"schedule"` // "seg", "ter", "qua", "qui", "sex", "sab", "dom"
}

type DiningTable struct {
	ID           int     `json:"id"`
	Number       int     `json:"number"`
	Label        string  `json:"label"`         // Ex: "MESA 12" ou "MESA 12 - Na Calçada"
	Complement   string  `json:"complement"`    // Complemento editável pelo Garçom (ex: "Na Calçada", "Deck")
	OriginPrefix string  `json:"origin_prefix"` // "[T]" (Mesas 01..12 Tablet Kiosk Goomer) ou "[Q]" (Mesas 13..30 Salão/QR)
	IsKiosk      bool    `json:"is_kiosk"`      // true = Mesas 01 a 12 (Cardápio Tablet Estilo Goomer)
	IsCustom     bool    `json:"is_custom"`
	Active       bool    `json:"active"`      // true = Mesa ativa/visível para a equipe; false = Mesa inativa pelo ADM
	Status       string  `json:"status"`      // LIVRE, OCUPADA, CONTA
	WaiterCall   bool    `json:"waiter_call"` // true quando o cliente chama o garçom no Tablet Kiosk
	QRToken      string  `json:"qr_token"`
	CurrentTotal float64 `json:"current_total"`
	UpdatedAt    string  `json:"updated_at"`
}

type SelectedModifier struct {
	GroupName  string  `json:"group_name"`
	OptionName string  `json:"option_name"`
	Price      float64 `json:"price"`
}

type OrderItem struct {
	ID                int                `json:"id"`
	OrderID           int                `json:"order_id"`
	ProductID         int                `json:"product_id"`
	ProductName       string             `json:"product_name"`
	Quantity          float64            `json:"quantity"`
	UnitPrice         float64            `json:"unit_price"`
	TotalPrice        float64            `json:"total_price"`
	SelectedModifiers []SelectedModifier `json:"selected_modifiers"`
	Notes             string             `json:"notes"`
	SectorABC         string             `json:"sector_abc"`
}

type SectorTicketPreview struct {
	StationID   string `json:"station_id"`
	StationName string `json:"station_name"`
	PrinterIP   string `json:"printer_ip"`
	Columns     int    `json:"columns"`
	ItemsCount  int    `json:"items_count"`
	TicketText  string `json:"ticket_text"`
}

type Order struct {
	ID              int                   `json:"id"`
	OrderCode       string                `json:"order_code"`
	OriginType      string                `json:"origin_type"`      // DELIVERY, MESA, GARCOM, TABLET, PDV_MERCADO
	HeaderTag       string                `json:"header_tag"`       // "[DELIVERY]", "MESA 01 • CMD-001", "MESA 15", "[PDV MERCADO]"
	TableID         *int                  `json:"table_id"`
	ComandaID       *int                  `json:"comanda_id"`
	ComandaCode     string                `json:"comanda_code"`     // Ex: "CMD-001" (QR Code Entrada) ou "NOME+CELULAR" (Mesas 13..30)
	CustomerName    string                `json:"customer_name"`
	CustomerPhone   string                `json:"customer_phone"`
	AddressText     string                `json:"address_text"`
	Latitude        float64               `json:"latitude"`
	Longitude       float64               `json:"longitude"`
	Status          string                `json:"status"`           // RECEBIDO, PREPARANDO, PRONTO, DESPACHADO, ENTREGUE, CANCELADO
	PaymentMethod   string                `json:"payment_method"`   // DINHEIRO, PIX_ENTREGA, CARTAO_ENTREGA, PIX_ONLINE
	GatewayID       string                `json:"gateway_id"`       // Preparado Fase 2 (Mercado Pago)
	StatusPagamento string                `json:"status_pagamento"` // PENDENTE, PAGO
	ChangeFor       float64               `json:"change_for"`
	Subtotal        float64               `json:"subtotal"`
	DeliveryFee     float64               `json:"delivery_fee"`
	CouponCode      string                `json:"coupon_code"`
	DiscountAmount  float64               `json:"discount_amount"`
	Total           float64               `json:"total"`
	MotoboyID       *int                  `json:"motoboy_id"`
	MotoboyName     string                `json:"motoboy_name"`
	WaiterID        *int                  `json:"waiter_id,omitempty"`
	WaiterName      string                `json:"waiter_name,omitempty"`
	CashierID       *int                  `json:"cashier_id,omitempty"`
	CashierName     string                `json:"cashier_name,omitempty"`
	ShiftID         int                   `json:"shift_id,omitempty"`
	QRPayload       string                `json:"qr_payload"`
	TrackingURL     string                `json:"tracking_url,omitempty"`
	Items           []OrderItem           `json:"items"`
	ReceiptPreview  string                `json:"receipt_preview,omitempty"`
	SectorTickets   []SectorTicketPreview `json:"sector_tickets,omitempty"`
	CreatedAt       string                `json:"created_at"`
	CreatedAtISO    string                `json:"created_at_iso,omitempty"`
	UpdatedAt       string                `json:"updated_at"`
}

type Comanda struct {
	ID            int         `json:"id"`
	ComandaCode   string      `json:"comanda_code"`   // Ex: "CMD-001" (QR Code Entrada) ou "M15-11999998888"
	ComandaType   string      `json:"comanda_type"`   // "QR_TABLET" (Cartão QR Entrada) ou "NOME_CELULAR" (Mesas sem tablet 13..30)
	TableID       int         `json:"table_id"`
	TableNumber   int         `json:"table_number"`
	TableLabel    string      `json:"table_label"`
	CustomerName  string      `json:"customer_name"`
	CustomerPhone string      `json:"customer_phone"`
	Status        string      `json:"status"`         // "ABERTA", "FECHAMENTO_SOLICITADO", "PAGA"
	PaymentMethod string      `json:"payment_method"`
	Total         float64     `json:"total"`
	OrdersCount   int         `json:"orders_count"`
	Items         []OrderItem `json:"items"`
	OpenedAt      string      `json:"opened_at"`
	ClosedAt      string      `json:"closed_at"`
}

type ComandaCard struct {
	CardNumber  int    `json:"card_number"`  // 1 a 100 (impresso no cartão PVC preto: 001..100)
	ComandaCode string `json:"comanda_code"` // "CMD-001" .. "CMD-100"
	QRPayload   string `json:"qr_payload"`   // Ex: UUID v4 "a1f5803f-0287-4944-83b4-a887422377b2" do cartão 001
	UpdatedAt   string `json:"updated_at"`
}

type MotoboyLedgerEntry struct {
	ID          int     `json:"id"`
	MotoboyID   int     `json:"motoboy_id"`
	MotoboyName string  `json:"motoboy_name"`
	OrderID     int     `json:"order_id"`
	EntryType   string  `json:"entry_type"` // DEBITO_DINHEIRO_PEDIDO, CREDITO_TAXA_ENTREGA, ACERTO_CAIXA
	Amount      float64 `json:"amount"`     // Positivo = motoboy recebe; Negativo = motoboy deve repassar ao caixa
	Description string  `json:"description"`
	CreatedAt   string  `json:"created_at"`
}

type MotoboySettlementSummary struct {
	MotoboyID          int                  `json:"motoboy_id"`
	MotoboyName        string               `json:"motoboy_name"`
	TotalDeliveries    int                  `json:"total_deliveries"`
	CashCollected      float64              `json:"cash_collected"`       // Dinheiro dos lanches na mão do motoboy
	DeliveryFeesEarned float64              `json:"delivery_fees_earned"` // Taxas das entregas creditadas ao motoboy
	NetBalanceToHouse  float64              `json:"net_balance_to_house"` // Quanto o motoboy deve entregar ao caixa no acerto
	Entries            []MotoboyLedgerEntry `json:"entries"`
}

type MotoboyShift struct {
	ID              int     `json:"id"`
	MotoboyID       int     `json:"motoboy_id"`
	MotoboyName     string  `json:"motoboy_name"`
	Status          string  `json:"status"`           // ABERTO, FECHADO
	OpeningFloat    float64 `json:"opening_float"`    // Troco inicial entregue ao motoboy na abertura do caixa
	DeliveriesCount int     `json:"deliveries_count"` // Total de entregas despachadas para este motoboy
	CashOrdersTotal float64 `json:"cash_orders_total"`
	CardPixTotal    float64 `json:"card_pix_total"`
	FeesEarned      float64 `json:"fees_earned"`
	NetToReturn     float64 `json:"net_to_return"` // (OpeningFloat + CashOrdersTotal) - FeesEarned
	OpenedAt        string  `json:"opened_at"`
	ClosedAt        string  `json:"closed_at"`
	OrdersList      []Order `json:"orders_list"`
}

type WhatsAppMessageLog struct {
	ID           int    `json:"id"`
	Phone        string `json:"phone"`
	CustomerName string `json:"customer_name"`
	Direction    string `json:"direction"` // INBOUND, OUTBOUND
	TriggerEvent string `json:"trigger_event"`
	MessageText  string `json:"message_text"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
}

type SystemLog struct {
	ID         int    `json:"id"`
	Level      string `json:"level"` // INFO, WARN, ERROR, CRASH_KOTLIN
	Module     string `json:"module"`
	Message    string `json:"message"`
	StackTrace string `json:"stack_trace"`
	DeviceInfo string `json:"device_info"`
	CreatedAt  string `json:"created_at"`
}

type DayOperatingHours struct {
	DayOfWeek int    `json:"day_of_week"` // 0 ou 7=Domingo, 1=Segunda, 2=Terça, 3=Quarta, 4=Quinta, 5=Sexta, 6=Sábado
	DayName   string `json:"day_name"`
	Enabled   bool   `json:"enabled"`
	IsOpen    bool   `json:"is_open"`
	OpenTime  string `json:"open_time"`  // Ex: "18:00"
	CloseTime string `json:"close_time"` // Ex: "23:30"
}

type OperatingHoursConfig struct {
	AutoScheduleEnabled bool                `json:"auto_schedule_enabled"`
	ManualOverrideMode  string              `json:"manual_override_mode"` // "AUTO", "FORCE_OPEN", "FORCE_CLOSED"
	Days                []DayOperatingHours `json:"days"`
}

type PrinterConfig struct {
	Columns                 int                  `json:"columns"`            // 32 (58mm) ou 48 (80mm)
	PaperWidthMM            int                  `json:"paper_width_mm"`     // 58 ou 80
	PrinterIP               string               `json:"printer_ip"`         // Ex: 192.168.1.200
	PrinterPort             int                  `json:"printer_port"`       // Padrão 9100
	AutoPrintOnAccept       bool                 `json:"auto_print_on_accept"`
	WhatsappBotPhase        int                  `json:"whatsapp_bot_phase"` // 1 (Estático) ou 2 (Gemini IA)
	GeminiAPIKey            string               `json:"gemini_api_key"`
	// Alarmes do Garçom (Toca/Vibra 3x -> Aguarda 1 min em loop até confirmar) + Supressão automática da impressora de bebidas
	WaiterAlarmTableCall    bool                 `json:"waiter_alarm_table_call"`    // 1. Chamado de Cliente na Mesa
	WaiterAlarmDrinks       bool                 `json:"waiter_alarm_drinks"`        // 2. Pedidos de Bebidas (Enquanto ativo, NÃO imprime bebidas na impressora!)
	WaiterAlarmKitchenReady bool                 `json:"waiter_alarm_kitchen_ready"` // 3. Pedido Pronto na Cozinha (Acionado pela Expedição)
	WaiterAlarmMode         string               `json:"waiter_alarm_mode"`          // "TOCAR_E_VIBRAR" ou "SO_VIBRAR"
	// Ativação / Desativação de Modalidades do Cardápio Digital pelo ADM
	MenuEnableDelivery      bool                 `json:"menu_enable_delivery"`
	MenuEnableRetirada      bool                 `json:"menu_enable_retirada"`
	MenuEnableMesa          bool                 `json:"menu_enable_mesa"`
	MenuEnableKiosk         bool                 `json:"menu_enable_kiosk"`
	MenuEnablePromotions    bool                 `json:"menu_enable_promotions"`
	// Controle de Categorias Visíveis para o Garçom (Sem Boutique/Mercado por padrão)
	WaiterVisibleCategories []int                `json:"waiter_visible_categories"`
	WaiterHiddenCategories  []int                `json:"waiter_hidden_categories"`
	// Taxa de Entrega Padrão (Exclusivo ADM) + Permissão para Caixas e Atendentes alterarem ou zerarem a Taxa
	DefaultDeliveryFee        float64              `json:"default_delivery_fee"`
	AllowStaffEditDeliveryFee bool                 `json:"allow_staff_edit_delivery_fee"`
	// Dias e Horários de Funcionamento (Abertura e Fechamento Automático pelo Programa)
	OperatingHours          OperatingHoursConfig `json:"operating_hours"`
	IsOpenNow               bool                 `json:"is_open_now"`
	ClosedReasonMessage     string               `json:"closed_reason_message"`
	TodayHoursLabel         string               `json:"today_hours_label"`
	// Estrutura pronta para o futuro: Atendimento & Pedidos por IA (WhatsApp) + Emissão NFC-e SEFAZ (Certificado A1)
	AIOrderBotEnabled       bool                 `json:"ai_order_bot_enabled"`
	AIProvider              string               `json:"ai_provider"`            // "GEMINI_2_5_FLASH", "GEMINI_2_5_PRO", "OPENAI_GPT4O_MINI"
	AICanCreateOrders       bool                 `json:"ai_can_create_orders"`   // Se true, permite à IA fechar pedidos completos no WhatsApp
	AIIncludeBoutique       bool                 `json:"ai_include_boutique"`    // Se false (padrão), IA atende e sugere apenas Hamburgueria; se true, inclui Boutique/Mercado
	AICustomInstructions    string               `json:"ai_custom_instructions"` // Instruções / regras da loja para a IA
	MercadoPagoEnabled      bool                 `json:"mp_enabled"`
	MercadoPagoToken        string               `json:"mp_access_token"`
	HoldKitchenUntilPaid    bool                 `json:"hold_kitchen_until_paid"`
	NFCeEnabled             bool                 `json:"nfce_enabled"`
	NFCeAmbiente            string               `json:"nfce_ambiente"` // HOMOLOGACAO ou PRODUCAO
	CNPJEmitente            string               `json:"cnpj_emitente"`
	NFCeRazaoSocial         string               `json:"nfce_razao_social"`
	NFCeInscricaoEstadual   string               `json:"nfce_inscricao_estadual"`
	NFCeRegimeTributario    string               `json:"nfce_regime_tributario"` // SIMPLES_NACIONAL (CRT 1) ou REGIME_NORMAL (CRT 3)
	NFCeEnderecoFiscal      string               `json:"nfce_endereco_fiscal"`
	NFCeSerie               int                  `json:"nfce_serie"`
	NFCeProximoNumero       int                  `json:"nfce_proximo_numero"`
	NFCeCSCID               string               `json:"nfce_csc_id"`
	NFCeCSCToken            string               `json:"nfce_csc_token"`
	NFCeCertFilename        string               `json:"nfce_cert_filename"`
	NFCeCertBase64          string               `json:"nfce_cert_base64"`
	NFCeCertPassword        string               `json:"nfce_cert_password"`
	NFCeAPIToken            string               `json:"nfce_api_token"`
}

type ABCItem struct {
	ProductID   int     `json:"product_id"`
	ProductName string  `json:"product_name"`
	SectorABC   string  `json:"sector_abc"`
	QtySold     float64 `json:"qty_sold"`
	Revenue     float64 `json:"revenue"`
	SharePct    float64 `json:"share_pct"`
	CumPct      float64 `json:"cum_pct"`
	Class       string  `json:"class"` // A, B, C
}

type FinanceDashboard struct {
	GrossRevenue        float64   `json:"gross_revenue"`
	BurgerRevenue       float64   `json:"burger_revenue"`
	MarketMeatRevenue   float64   `json:"market_meat_revenue"`
	DeliveryFeesTotal   float64   `json:"delivery_fees_total"`
	EstimatedCMV        float64   `json:"estimated_cmv"`
	GrossProfit         float64   `json:"gross_profit"`
	GrossMarginPct      float64   `json:"gross_margin_pct"`
	OrdersCount         int       `json:"orders_count"`
	AverageTicket       float64   `json:"average_ticket"`
	ABCBurgers          []ABCItem `json:"abc_burgers"`
	ABCMarketAndButcher []ABCItem `json:"abc_market_and_butcher"`
}

type PrinterStation struct {
	ID                 string `json:"id"`                   // COZINHA_CHAPA, COZINHA_PORCOES, BAR_COPA, CAIXA_EXPEDICAO, ACOUGUE_MERCADO ou custom
	Name               string `json:"name"`                 // Ex: "🍔 Cozinha — Lanches (Chapa)"
	SectorLabel        string `json:"sector_label"`         // Ex: "COZINHA - LANCHES"
	ConnectionType     string `json:"connection_type"`      // USB_RAW, BLUETOOTH, REDE_COMPARTILHADA, REDE_ESC_POS
	PCName             string `json:"pc_name"`              // "PC_COZINHA" (Local Win11) ou "PC_CAIXA" (Remoto Win10)
	WindowsPrinterName string `json:"windows_printer_name"` // Nome exato no Spooler do Windows (Autodetectado)
	PrinterIP          string `json:"printer_ip"`           // Ex: "192.168.18.201" ou nome da impressora Windows
	IPAddress          string `json:"ip_address"`           // Alias JSON p/ compatibilidade com PDV Web
	PrinterPort        int    `json:"printer_port"`         // 9100
	Port               int    `json:"port"`                 // Alias JSON p/ compatibilidade com PDV Web
	Columns            int    `json:"columns"`              // 32 ou 48
	PaperWidthMM       int    `json:"paper_width_mm"`       // 58 ou 80
	CutPaperSeparately bool   `json:"cut_paper_separately"` // Envia guilhotina (GS V) separada por setor (ex: Lanche vs Porção na mesma impressora)
	PrintWaiterCalls   bool   `json:"print_waiter_calls"`   // ☑️ Imprimir Chamados de Garçom / Pediu a Conta (ex: Bluetooth do Caixa)
	PrintDrinks        bool   `json:"print_drinks"`         // ☑️ Imprimir Pedidos de Bebidas (ex: Bluetooth do Caixa)
	PrintBillClosure   bool   `json:"print_bill_closure"`   // ☑️ Imprimir Fechamento de Contas / Mesas (ex: Rede do Caixa)
	PrintBoutique      bool   `json:"print_boutique"`       // ☑️ Imprimir Cupons da Boutique / Mercado (ex: Rede do Caixa)
	PrintFiscalNotes   bool   `json:"print_fiscal_notes"`   // ☑️ Imprimir Contas / Expedição / Notas Fiscais NFC-e (ex: Rede do Caixa)
	Active             bool   `json:"active"`
	Enabled            bool   `json:"enabled"` // Alias JSON p/ compatibilidade com PDV Web
}

type DetectedPrinter struct {
	ID                 string `json:"id"`                   // Identificador único
	Key                string `json:"key"`                  // Identificador único: PCName + "|" + WindowsPrinterName
	Name               string `json:"name"`                 // Alias p/ PDV Web
	PrinterName        string `json:"printer_name"`         // Alias p/ APK
	PCName             string `json:"pc_name"`              // "PC_COZINHA" (Windows 11) ou "PC_CAIXA" (Windows 10)
	PCRoleLabel        string `json:"pc_role_label"`        // Ex: "🖥️ PC Cozinha (Servidor Local)" ou "💻 PC Caixa (Ponte Remota)"
	WindowsPrinterName string `json:"windows_printer_name"` // Ex: "EPSON TM-T20 Receipt", "POS-80", "Bluetooth Printer"
	PortName           string `json:"port_name"`            // Ex: "USB001", "BTH001 / COM4", "IP_192.168.18.200", "\\CAIXA\POS80"
	DriverName         string `json:"driver_name"`
	ConnectionType     string `json:"connection_type"`      // USB_RAW, BLUETOOTH, REDE_COMPARTILHADA, REDE_ESC_POS
	ConnectionBadge    string `json:"connection_badge"`     // "🔌 USB", "📶 Bluetooth", "🌐 Rede Compartilhada"
	Shared             bool   `json:"shared"`
	ShareName          string `json:"share_name"`
	Status             string `json:"status"`               // "ONLINE", "PRONTA"
	LastSeenAt         string `json:"last_seen_at"`
	IsRemotePC         bool   `json:"is_remote_pc"`
}

type RemotePrintJob struct {
	ID                 string `json:"id"`
	PCName             string `json:"pc_name"`
	TargetPCName       string `json:"target_pc_name"`
	WindowsPrinterName string `json:"windows_printer_name"`
	PrinterIP          string `json:"printer_ip"`
	PrinterPort        int    `json:"printer_port"`
	StationID          string `json:"station_id"`
	StationName        string `json:"station_name"`
	JobType            string `json:"job_type"` // "SECTOR_TICKET", "WAITER_CALL", "BILL_CLOSURE", "BOUTIQUE", "FISCAL_NFCE", "TEST"
	TicketText         string `json:"ticket_text"`
	TextContent        string `json:"text_content"`
	RawBase64          string `json:"raw_base64"`
	CutPaper           bool   `json:"cut_paper"`
	Columns            int    `json:"columns"`
	CreatedAt          string `json:"created_at"`
}

type CategoryPrinterRoute struct {
	CategoryID         int    `json:"category_id"`
	CategoryName       string `json:"category_name"`
	PrinterStationID   string `json:"printer_station_id"`
	PrinterStationName string `json:"printer_station_name"`
	PrinterName        string `json:"printer_name"` // Alias JSON p/ compatibilidade com PDV Web
}

type CashierShift struct {
	ID                 int     `json:"id"`
	CashierID          int     `json:"cashier_id"`
	CashierName        string  `json:"cashier_name"`
	CashierUsername    string  `json:"cashier_username"`
	Status             string  `json:"status"`               // ABERTO, FECHADO
	OpeningFloat       float64 `json:"opening_float"`        // Fundo de Troco Inicial definido pelo ADM
	SalesCount         int     `json:"sales_count"`          // Quantidade de vendas/comandas recebidas neste turno
	CashSalesTotal     float64 `json:"cash_sales_total"`     // Total recebido em Dinheiro
	PixSalesTotal      float64 `json:"pix_sales_total"`      // Total recebido em PIX
	CardSalesTotal     float64 `json:"card_sales_total"`     // Total recebido em Cartão
	ExpectedDrawerCash float64 `json:"expected_drawer_cash"` // OpeningFloat + CashSalesTotal
	TotalMoved         float64 `json:"total_moved"`          // Cash + Pix + Card
	CountedCash        float64 `json:"counted_cash"`         // Valor conferido em Dinheiro no fechamento ADM
	CountedPix         float64 `json:"counted_pix"`          // Valor conferido em PIX no fechamento ADM
	CountedCard        float64 `json:"counted_card"`         // Valor conferido em Cartão no fechamento ADM
	OpenedAt           string  `json:"opened_at"`
	OpenedAtISO        string  `json:"opened_at_iso"`
	ClosedAt           string  `json:"closed_at"`
	ClosedAtISO        string  `json:"closed_at_iso"`
	OpenedByAdmin      string  `json:"opened_by_admin"`
	ClosedByAdmin      string  `json:"closed_by_admin"`
}

type Customer struct {
	ID               int     `json:"id"`
	Name             string  `json:"name"`
	Phone            string  `json:"phone"`
	OriginChannel    string  `json:"origin_channel"` // DELIVERY, RETIRADA, AMBOS
	Street           string  `json:"street"`
	Number           string  `json:"number"`
	Neighborhood     string  `json:"neighborhood"`
	Complement       string  `json:"complement"`
	AddressText      string  `json:"address_text"`
	Latitude         float64 `json:"latitude"`
	Longitude        float64 `json:"longitude"`
	Notes            string  `json:"notes"`
	TotalSpent       float64 `json:"total_spent"`
	OrdersCount      int     `json:"orders_count"`
	RewardsEarned    int     `json:"rewards_earned"`
	RewardsRedeemed  int     `json:"rewards_redeemed"`
	RewardsAvailable int     `json:"rewards_available"`
	RewardThreshold  float64 `json:"reward_threshold"`
	ProgressToNext   float64 `json:"progress_to_next"`
	RemainingToNext  float64 `json:"remaining_to_next"`
	LastOrderAt      string  `json:"last_order_at"`
	CreatedAt        string  `json:"created_at"`
	OrdersHistory    []Order `json:"orders_history"`
}

type WaiterAlert struct {
	ID           int         `json:"id"`
	AlertType    string      `json:"alert_type"` // "CHAMADO_MESA", "BEBIDAS_MESA", "PRONTO_COZINHA"
	TableID      int         `json:"table_id"`
	TableNumber  int         `json:"table_number"`
	TableLabel   string      `json:"table_label"`
	OrderID      int         `json:"order_id"`
	OrderCode    string      `json:"order_code"`
	ComandaCode  string      `json:"comanda_code"`
	CustomerName string      `json:"customer_name"`
	Title        string      `json:"title"`
	Details      string      `json:"details"`
	Items        []OrderItem `json:"items"`
	Status       string      `json:"status"` // "PENDENTE", "CONFIRMADO"
	CreatedAt    string      `json:"created_at"`
	ConfirmedBy  string      `json:"confirmed_by"`
	ConfirmedAt  string      `json:"confirmed_at"`
}

type DiscountCoupon struct {
	ID            int     `json:"id"`
	Code          string  `json:"code"`           // Ex: "SIRBEEF10", "VIP-JULIO"
	CouponType    string  `json:"coupon_type"`    // "FIXO" (múltiplos usos c/ ligar/desligar) ou "USO_UNICO" (queima após 1 uso)
	DiscountType  string  `json:"discount_type"`  // "VALOR" (R$) ou "PERCENTUAL" (%)
	DiscountValue float64 `json:"discount_value"` // Ex: 15.00 (R$ 15) ou 10.0 (10%)
	CustomerID    int     `json:"customer_id"`
	CustomerName  string  `json:"customer_name"`
	CustomerPhone string  `json:"customer_phone"`
	Reason        string  `json:"reason"`
	Active        bool    `json:"active"`
	UsedCount     int     `json:"used_count"`
	UsedAt        string  `json:"used_at"`
	UsedOrderID   int     `json:"used_order_id"`
	CreatedAt     string  `json:"created_at"`
}

type MotoboyLiveLocation struct {
	MotoboyID     int     `json:"motoboy_id"`
	MotoboyName   string  `json:"motoboy_name"`
	Latitude      float64 `json:"latitude"`
	Longitude     float64 `json:"longitude"`
	Accuracy      float64 `json:"accuracy"`
	Speed         float64 `json:"speed"`
	Heading       float64 `json:"heading"`
	UpdatedAt     string  `json:"updated_at"`
	UpdatedAtUnix int64   `json:"updated_at_unix"`
	SecondsAgo    int64   `json:"seconds_ago"`
	IsLive        bool    `json:"is_live"`
}


