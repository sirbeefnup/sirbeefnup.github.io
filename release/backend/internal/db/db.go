package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	"sirbeef/backend/internal/escpos"
	"sirbeef/backend/internal/models"
	"sirbeef/backend/internal/whatsapp"
)

var (
	pendingWhatsAppMu     sync.Mutex
	pendingWhatsAppTimers = make(map[int]*time.Timer)
)

func orderStageRank(st string) int {
	switch strings.ToUpper(strings.TrimSpace(st)) {
	case "RECEBIDO":
		return 1
	case "PREPARANDO":
		return 2
	case "PRONTO":
		return 3
	case "DESPACHADO":
		return 4
	case "ENTREGUE":
		return 5
	default:
		return 0
	}
}

type Store struct {
	DB       *sql.DB
	WhatsApp *whatsapp.Manager
}

func OpenStore(dbPath string, seedJSONPath string) (*Store, error) {
	conn, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	// Ativa modo WAL para altíssima performance e concorrência local no PDV
	_, _ = conn.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=ON;`)

	s := &Store{DB: conn}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	if err := s.seedIfEmpty(seedJSONPath); err != nil {
		return nil, err
	}
	s.CleanupZeroOrphanComandas()
	return s, nil
}

func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		username TEXT NOT NULL UNIQUE,
		pin TEXT NOT NULL,
		role TEXT NOT NULL,
		active INTEGER NOT NULL DEFAULT 1,
		status TEXT NOT NULL DEFAULT 'APROVADO',
		created_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS categories (
		id INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		sector TEXT NOT NULL DEFAULT 'HAMBURGUERIA',
		sector_abc TEXT NOT NULL DEFAULT 'HAMBURGUERIA'
	);

	CREATE TABLE IF NOT EXISTS products (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		external_id INTEGER NOT NULL DEFAULT 0,
		category_id INTEGER NOT NULL,
		category_name TEXT NOT NULL,
		sector TEXT NOT NULL DEFAULT 'HAMBURGUERIA',
		sector_abc TEXT NOT NULL DEFAULT 'HAMBURGUERIA',
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		price REAL NOT NULL DEFAULT 0,
		promo_from REAL,
		cost_price REAL NOT NULL DEFAULT 0,
		ean13 TEXT NOT NULL DEFAULT '',
		plu_code TEXT NOT NULL DEFAULT '',
		is_weighted INTEGER NOT NULL DEFAULT 0,
		ncm TEXT NOT NULL DEFAULT '21069090',
		cest TEXT NOT NULL DEFAULT '1709900',
		cfop TEXT NOT NULL DEFAULT '5102',
		image_url TEXT NOT NULL DEFAULT '',
		modifier_groups_json TEXT NOT NULL DEFAULT '[]',
		stock_qty REAL NOT NULL DEFAULT 40.0,
		min_stock REAL NOT NULL DEFAULT 5.0,
		track_stock INTEGER NOT NULL DEFAULT 1,
		active INTEGER NOT NULL DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS dining_tables (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		number INTEGER NOT NULL,
		label TEXT NOT NULL,
		complement TEXT NOT NULL DEFAULT '',
		origin_prefix TEXT NOT NULL DEFAULT '[Q]',
		is_kiosk INTEGER NOT NULL DEFAULT 0,
		is_custom INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'LIVRE',
		waiter_call INTEGER NOT NULL DEFAULT 0,
		qr_token TEXT NOT NULL,
		current_total REAL NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS orders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		order_code TEXT NOT NULL,
		origin_type TEXT NOT NULL,
		header_tag TEXT NOT NULL,
		table_id INTEGER,
		customer_name TEXT NOT NULL DEFAULT '',
		customer_phone TEXT NOT NULL DEFAULT '',
		address_text TEXT NOT NULL DEFAULT '',
		latitude REAL NOT NULL DEFAULT -23.55052,
		longitude REAL NOT NULL DEFAULT -46.63331,
		status TEXT NOT NULL DEFAULT 'RECEBIDO',
		payment_method TEXT NOT NULL DEFAULT 'DINHEIRO',
		gateway_id TEXT NOT NULL DEFAULT '',
		status_pagamento TEXT NOT NULL DEFAULT 'PENDENTE',
		change_for REAL NOT NULL DEFAULT 0,
		subtotal REAL NOT NULL DEFAULT 0,
		delivery_fee REAL NOT NULL DEFAULT 0,
		total REAL NOT NULL DEFAULT 0,
		motoboy_id INTEGER,
		motoboy_name TEXT NOT NULL DEFAULT '',
		qr_payload TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS comandas (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		comanda_code TEXT NOT NULL,
		comanda_type TEXT NOT NULL DEFAULT 'QR_TABLET',
		table_id INTEGER NOT NULL DEFAULT 1,
		table_number INTEGER NOT NULL DEFAULT 1,
		table_label TEXT NOT NULL DEFAULT 'MESA 01',
		customer_name TEXT NOT NULL DEFAULT '',
		customer_phone TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'ABERTA',
		payment_method TEXT NOT NULL DEFAULT 'PENDENTE',
		total REAL NOT NULL DEFAULT 0,
		orders_count INTEGER NOT NULL DEFAULT 0,
		opened_at TEXT NOT NULL DEFAULT '',
		closed_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS comanda_cards (
		card_number INTEGER PRIMARY KEY,
		comanda_code TEXT NOT NULL UNIQUE,
		qr_payload TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS order_items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		order_id INTEGER NOT NULL,
		product_id INTEGER NOT NULL,
		product_name TEXT NOT NULL,
		quantity REAL NOT NULL DEFAULT 1,
		unit_price REAL NOT NULL DEFAULT 0,
		total_price REAL NOT NULL DEFAULT 0,
		selected_modifiers_json TEXT NOT NULL DEFAULT '[]',
		notes TEXT NOT NULL DEFAULT '',
		sector_abc TEXT NOT NULL DEFAULT 'HAMBURGUERIA'
	);

	CREATE TABLE IF NOT EXISTS motoboy_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		motoboy_id INTEGER NOT NULL,
		motoboy_name TEXT NOT NULL,
		order_id INTEGER NOT NULL,
		entry_type TEXT NOT NULL,
		amount REAL NOT NULL,
		description TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS motoboy_shifts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		motoboy_id INTEGER NOT NULL UNIQUE,
		motoboy_name TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'FECHADO',
		opening_float REAL NOT NULL DEFAULT 0,
		opened_at TEXT NOT NULL DEFAULT '',
		closed_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS whatsapp_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		phone TEXT NOT NULL,
		customer_name TEXT NOT NULL,
		direction TEXT NOT NULL,
		trigger_event TEXT NOT NULL,
		message_text TEXT NOT NULL,
		status TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS system_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		level TEXT NOT NULL,
		module TEXT NOT NULL,
		message TEXT NOT NULL,
		stack_trace TEXT NOT NULL,
		device_info TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS printer_config (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		columns INTEGER NOT NULL DEFAULT 48,
		paper_width_mm INTEGER NOT NULL DEFAULT 80,
		printer_ip TEXT NOT NULL DEFAULT '192.168.1.200',
		printer_port INTEGER NOT NULL DEFAULT 9100,
		auto_print_on_accept INTEGER NOT NULL DEFAULT 1,
		whatsapp_bot_phase INTEGER NOT NULL DEFAULT 1,
		gemini_api_key TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS printer_stations (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		printer_ip TEXT NOT NULL DEFAULT '',
		printer_port INTEGER NOT NULL DEFAULT 9100,
		columns INTEGER NOT NULL DEFAULT 48,
		paper_width_mm INTEGER NOT NULL DEFAULT 80,
		active INTEGER NOT NULL DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS category_printer_routes (
		category_id INTEGER PRIMARY KEY,
		category_name TEXT NOT NULL DEFAULT '',
		printer_station_id TEXT NOT NULL DEFAULT 'COZINHA_CHAPA'
	);

	CREATE TABLE IF NOT EXISTS cashier_shifts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		cashier_id INTEGER NOT NULL UNIQUE,
		cashier_name TEXT NOT NULL DEFAULT '',
		cashier_username TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'FECHADO',
		opening_float REAL NOT NULL DEFAULT 0,
		sales_count INTEGER NOT NULL DEFAULT 0,
		cash_sales_total REAL NOT NULL DEFAULT 0,
		pix_sales_total REAL NOT NULL DEFAULT 0,
		card_sales_total REAL NOT NULL DEFAULT 0,
		counted_cash REAL NOT NULL DEFAULT 0,
		counted_pix REAL NOT NULL DEFAULT 0,
		counted_card REAL NOT NULL DEFAULT 0,
		opened_at TEXT NOT NULL DEFAULT '',
		opened_at_iso TEXT NOT NULL DEFAULT '',
		closed_at TEXT NOT NULL DEFAULT '',
		closed_at_iso TEXT NOT NULL DEFAULT '',
		opened_by_admin TEXT NOT NULL DEFAULT '',
		closed_by_admin TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS customers (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT '',
		phone TEXT NOT NULL DEFAULT '',
		origin_channel TEXT NOT NULL DEFAULT 'DELIVERY',
		street TEXT NOT NULL DEFAULT '',
		number TEXT NOT NULL DEFAULT '',
		neighborhood TEXT NOT NULL DEFAULT '',
		complement TEXT NOT NULL DEFAULT '',
		address_text TEXT NOT NULL DEFAULT '',
		latitude REAL NOT NULL DEFAULT -23.55052,
		longitude REAL NOT NULL DEFAULT -46.63331,
		notes TEXT NOT NULL DEFAULT '',
		total_spent REAL NOT NULL DEFAULT 0,
		orders_count INTEGER NOT NULL DEFAULT 0,
		rewards_redeemed INTEGER NOT NULL DEFAULT 0,
		reward_threshold REAL NOT NULL DEFAULT 150.0,
		last_order_at TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS waiter_alerts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		alert_type TEXT NOT NULL DEFAULT 'CHAMADO_MESA',
		table_id INTEGER NOT NULL DEFAULT 0,
		table_number INTEGER NOT NULL DEFAULT 0,
		table_label TEXT NOT NULL DEFAULT '',
		order_id INTEGER NOT NULL DEFAULT 0,
		order_code TEXT NOT NULL DEFAULT '',
		comanda_code TEXT NOT NULL DEFAULT '',
		customer_name TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		details TEXT NOT NULL DEFAULT '',
		items_json TEXT NOT NULL DEFAULT '[]',
		status TEXT NOT NULL DEFAULT 'PENDENTE',
		created_at TEXT NOT NULL DEFAULT '',
		confirmed_by TEXT NOT NULL DEFAULT '',
		confirmed_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS modifier_group_templates (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT '',
		min_select INTEGER NOT NULL DEFAULT 0,
		max_select INTEGER NOT NULL DEFAULT 1,
		required INTEGER NOT NULL DEFAULT 0,
		options_json TEXT NOT NULL DEFAULT '[]'
	);

	CREATE TABLE IF NOT EXISTS weekly_promotions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		day_of_week INTEGER NOT NULL DEFAULT 1,
		product_id INTEGER NOT NULL DEFAULT 0,
		original_price REAL NOT NULL DEFAULT 0,
		promo_price REAL NOT NULL DEFAULT 0,
		active INTEGER NOT NULL DEFAULT 1,
		updated_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS discount_coupons (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		code TEXT NOT NULL UNIQUE,
		coupon_type TEXT NOT NULL DEFAULT 'FIXO',
		discount_type TEXT NOT NULL DEFAULT 'VALOR',
		discount_value REAL NOT NULL DEFAULT 0,
		customer_id INTEGER NOT NULL DEFAULT 0,
		customer_name TEXT NOT NULL DEFAULT '',
		customer_phone TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		active INTEGER NOT NULL DEFAULT 1,
		used_count INTEGER NOT NULL DEFAULT 0,
		used_at TEXT NOT NULL DEFAULT '',
		used_order_id INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS motoboy_locations (
		motoboy_id INTEGER PRIMARY KEY,
		motoboy_name TEXT NOT NULL DEFAULT '',
		latitude REAL NOT NULL DEFAULT 0,
		longitude REAL NOT NULL DEFAULT 0,
		accuracy REAL NOT NULL DEFAULT 0,
		speed REAL NOT NULL DEFAULT 0,
		heading REAL NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT '',
		updated_at_unix INTEGER NOT NULL DEFAULT 0
	);
	`
	_, err := s.DB.Exec(schema)
	// Migração automática para bancos já existentes
	_, _ = s.DB.Exec(`ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'APROVADO'`)
	_, _ = s.DB.Exec(`ALTER TABLE users ADD COLUMN created_at TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE users ADD COLUMN table_number INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE users ADD COLUMN session_token TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`UPDATE users SET status = CASE WHEN active = 1 THEN 'APROVADO' ELSE 'PENDENTE' END WHERE status = ''`)
	_, _ = s.DB.Exec(`ALTER TABLE products ADD COLUMN stock_qty REAL NOT NULL DEFAULT 40.0`)
	_, _ = s.DB.Exec(`ALTER TABLE products ADD COLUMN min_stock REAL NOT NULL DEFAULT 5.0`)
	_, _ = s.DB.Exec(`ALTER TABLE products ADD COLUMN track_stock INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE dining_tables ADD COLUMN complement TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE dining_tables ADD COLUMN is_kiosk INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE dining_tables ADD COLUMN waiter_call INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE dining_tables ADD COLUMN active INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN comanda_id INTEGER`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN comanda_code TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN waiter_id INTEGER`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN waiter_name TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN cashier_id INTEGER`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN cashier_name TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN shift_id INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN created_at_iso TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN coupon_code TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE orders ADD COLUMN discount_amount REAL NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`UPDATE orders SET created_at_iso = strftime('%Y-%m-%d %H:%M:%S', 'now', 'localtime') WHERE TRIM(COALESCE(created_at_iso, '')) = ''`)
	_, _ = s.DB.Exec(`UPDATE orders SET waiter_id = 3, waiter_name = 'Lucas (Garçom Salão & Deck)' WHERE (UPPER(origin_type) = 'GARCOM' OR UPPER(origin_type) = 'MESA') AND TRIM(COALESCE(waiter_name, '')) = ''`)
	_, _ = s.DB.Exec(`UPDATE orders SET status = 'PREPARANDO' WHERE UPPER(status) = 'RECEBIDO'`)
	_, _ = s.DB.Exec(`ALTER TABLE cashier_shifts ADD COLUMN counted_cash REAL NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE cashier_shifts ADD COLUMN counted_pix REAL NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE cashier_shifts ADD COLUMN counted_card REAL NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE cashier_shifts ADD COLUMN opened_at_iso TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE cashier_shifts ADD COLUMN closed_at_iso TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN reward_threshold_brl REAL NOT NULL DEFAULT 150.0`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN waiter_alarm_table_call INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN waiter_alarm_drinks INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN waiter_alarm_kitchen_ready INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN waiter_alarm_mode TEXT NOT NULL DEFAULT 'TOCAR_E_VIBRAR'`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN menu_enable_delivery INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN menu_enable_retirada INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN menu_enable_mesa INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN menu_enable_kiosk INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN waiter_visible_categories_json TEXT NOT NULL DEFAULT '[]'`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN waiter_hidden_categories_json TEXT NOT NULL DEFAULT '["Boutique das Carnes & Empório"]'`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN default_delivery_fee REAL NOT NULL DEFAULT 8.0`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN allow_staff_edit_delivery_fee INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN operating_hours_json TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_config ADD COLUMN ai_nfce_settings_json TEXT NOT NULL DEFAULT '{}'`)

	// Novas colunas de Impressoras Autodetectáveis nos 2 PCs (Cozinha Win11 USB + Caixa Win10 Bluetooth/Rede) e Setores com Corte Separado
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN sector_label TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN connection_type TEXT NOT NULL DEFAULT 'USB'`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN pc_name TEXT NOT NULL DEFAULT 'PC-COZINHA'`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN windows_printer_name TEXT NOT NULL DEFAULT ''`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN cut_paper_separately INTEGER NOT NULL DEFAULT 1`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN print_waiter_calls INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN print_drinks INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN print_bill_closure INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN print_boutique INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.DB.Exec(`ALTER TABLE printer_stations ADD COLUMN print_fiscal_notes INTEGER NOT NULL DEFAULT 0`)

	// Corrige produtos em oferta onde o preço promocional (menor) estava salvo em promo_from (riscado) e o preço antigo (maior) estava em price
	_, _ = s.DB.Exec(`UPDATE products SET price = promo_from, promo_from = price WHERE promo_from IS NOT NULL AND promo_from > 0 AND promo_from < price`)

	// Remove categorias duplicadas (0 itens) e sincroniza nomes de categorias
	s.deduplicateCategories()

	// Garante os Grupos de Adicionais reutilizáveis e limpa "Ponto da Carne" das Porções / Batatas / Frango
	s.ensureDefaultModifierGroupTemplatesAndCleanPorcoes()

	// Garante as 30 Mesas padrão (01 a 12 = Tablet Kiosk sem QR de Mesa, 13 a 30 = Salão/QR) sem prefixo [Q]/[T]
	_, _ = s.DB.Exec(`DELETE FROM dining_tables WHERE number = 99`)
	now := time.Now().Format("15:04")
	for i := 1; i <= 30; i++ {
		var exists int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM dining_tables WHERE number = ?`, i).Scan(&exists)
		prefix := ""
		isKiosk := 0
		if i <= 12 {
			isKiosk = 1
		}
		if exists == 0 {
			label := fmt.Sprintf("MESA %02d", i)
			comp := ""
			if i == 12 {
				comp = "Na Calçada"
				label = `MESA 12 - "Na Calçada"`
			}
			token := fmt.Sprintf("SIRBEEF-MESA-%02d", i)
			if isKiosk == 1 {
				token = ""
			}
			_, _ = s.DB.Exec(`INSERT INTO dining_tables (number, label, complement, origin_prefix, is_kiosk, is_custom, status, waiter_call, qr_token, current_total, updated_at)
				VALUES (?, ?, ?, ?, ?, 0, 'LIVRE', 0, ?, 0, ?)`, i, label, comp, prefix, isKiosk, token, now)
		} else {
			_, _ = s.DB.Exec(`UPDATE dining_tables SET origin_prefix = '' WHERE number = ?`, i)
			if i == 12 {
				_, _ = s.DB.Exec(`UPDATE dining_tables SET complement = 'Na Calçada', label = 'MESA 12 - "Na Calçada"' WHERE number = 12 AND complement = ''`)
			}
		}
	}
	_, _ = s.DB.Exec(`UPDATE dining_tables SET origin_prefix = ''`)
	_, _ = s.DB.Exec(`UPDATE dining_tables SET qr_token = '' WHERE is_kiosk = 1`)
	_, _ = s.DB.Exec(`UPDATE dining_tables SET qr_token = printf('SIRBEEF-MESA-%02d', number) WHERE is_kiosk = 0 AND TRIM(COALESCE(qr_token, '')) = ''`)
	_, _ = s.DB.Exec(`UPDATE orders SET header_tag = REPLACE(REPLACE(header_tag, '[T] ', ''), '[Q] ', '')`)

	// Garante logins de Tablet Kiosk (mesa01 até mesa12, senha 1234) para abrir direto o Cardápio Estilo Goomer
	fullNow := time.Now().Format("02/01/2006 15:04")
	for i := 1; i <= 12; i++ {
		uName := fmt.Sprintf("mesa%02d", i)
		dName := fmt.Sprintf("Tablet Kiosk — Mesa %02d", i)
		_, _ = s.DB.Exec(`INSERT OR IGNORE INTO users (name, username, pin, role, table_number, active, status, created_at)
			VALUES (?, ?, '1234', 'TABLET', ?, 1, 'APROVADO', ?)`, dName, uName, i, fullNow)
		_, _ = s.DB.Exec(`UPDATE users SET table_number = ? WHERE LOWER(username) = LOWER(?) AND table_number = 0`, i, uName)
	}

	// Garante as 100 Comandas Físicas (001 a 100) — com o Cartão 001 já vinculado ao UUID real lido do cartão PVC Sir Beef
	for i := 1; i <= 100; i++ {
		cCode := fmt.Sprintf("CMD-%03d", i)
		defaultQR := ""
		if i == 1 {
			defaultQR = "a1f5803f-0287-4944-83b4-a887422377b2"
		}
		_, _ = s.DB.Exec(`INSERT OR IGNORE INTO comanda_cards (card_number, comanda_code, qr_payload, updated_at) VALUES (?, ?, ?, ?)`,
			i, cCode, defaultQR, fullNow)
		if i == 1 {
			_, _ = s.DB.Exec(`UPDATE comanda_cards SET qr_payload = ? WHERE card_number = 1 AND qr_payload = ''`, defaultQR)
		}
	}

	// QR Code de despacho deve existir SOMENTE nos pedidos de ENTREGA (DELIVERY)
	_, _ = s.DB.Exec(`UPDATE orders SET qr_payload = '' WHERE UPPER(origin_type) != 'DELIVERY'`)
	// Limpa códigos de comanda que tinham telefone concatenado (ex: M16-16998655666 -> M16)
	_, _ = s.DB.Exec(`UPDATE comandas SET comanda_code = SUBSTR(comanda_code, 1, INSTR(comanda_code, '-') - 1) WHERE comanda_code LIKE 'M%-%' AND INSTR(comanda_code, '-') > 1`)
	_, _ = s.DB.Exec(`UPDATE orders SET comanda_code = SUBSTR(comanda_code, 1, INSTR(comanda_code, '-') - 1) WHERE comanda_code LIKE 'M%-%' AND INSTR(comanda_code, '-') > 1`)
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO motoboy_shifts (motoboy_id, motoboy_name, status, opening_float, opened_at) VALUES (4, 'Rafael (Motoboy Express 01)', 'ABERTO', 50.0, ?)`, time.Now().Format("02/01 18:00"))
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO motoboy_shifts (motoboy_id, motoboy_name, status, opening_float, opened_at) VALUES (5, 'Diego (Motoboy Express 02)', 'FECHADO', 0.0, '')`)

	// Garante um segundo operador de Caixa com login próprio para demonstrar múltiplos funcionários de Caixa
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO users (name, username, pin, role, active, status, created_at)
		VALUES ('Bruno (Operador de Caixa 02)', 'bruno', '2323', 'CAIXA', 1, 'APROVADO', ?)`, fullNow)

	// Garante as Estações de Impressoras por Setor nos 2 PCs (PC Cozinha Win11 USB c/ corte separado Lanche x Porção + PC Caixa Win10 Bluetooth e Rede)
	defaultStations := []models.PrinterStation{
		{
			ID: "COZINHA_CHAPA", Name: "🍔 Cozinha — Lanches & Chapa [Papel Cortado Separado]",
			SectorLabel: "COZINHA - LANCHES (CHAPA)", ConnectionType: "USB", PCName: "PC-COZINHA",
			PrinterIP: "192.168.18.201", PrinterPort: 9100, Columns: 48, PaperWidthMM: 80,
			CutPaperSeparately: true, Active: true,
		},
		{
			ID: "COZINHA_PORCOES", Name: "🍟 Cozinha — Porções & Fritadeira [Papel Cortado Separado]",
			SectorLabel: "COZINHA - PORÇÕES (FRITADEIRA)", ConnectionType: "USB", PCName: "PC-COZINHA",
			PrinterIP: "192.168.18.201", PrinterPort: 9100, Columns: 48, PaperWidthMM: 80,
			CutPaperSeparately: true, Active: true,
		},
		{
			ID: "BAR_COPA", Name: "🍺 Caixa (Bluetooth) — Bebidas & Chamados de Garçom",
			SectorLabel: "BAR / BEBIDAS & GARÇOM", ConnectionType: "BLUETOOTH", PCName: "PC-CAIXA",
			PrinterIP: "192.168.18.202", PrinterPort: 9100, Columns: 48, PaperWidthMM: 80,
			CutPaperSeparately: true, PrintWaiterCalls: true, PrintDrinks: true, Active: true,
		},
		{
			ID: "CAIXA_EXPEDICAO", Name: "🧾 Caixa (Rede Compartilhada) — Contas, Boutique & Notas Fiscais",
			SectorLabel: "CAIXA / CONTAS / BOUTIQUE / NFC-E", ConnectionType: "REDE", PCName: "PC-CAIXA",
			PrinterIP: "192.168.18.200", PrinterPort: 9100, Columns: 48, PaperWidthMM: 80,
			CutPaperSeparately: true, PrintBillClosure: true, PrintBoutique: true, PrintFiscalNotes: true, Active: true,
		},
		{
			ID: "ACOUGUE_MERCADO", Name: "🥩 Boutique das Carnes & Mercado (Rota Opcional)",
			SectorLabel: "BOUTIQUE & MERCADO", ConnectionType: "REDE", PCName: "PC-CAIXA",
			PrinterIP: "192.168.18.203", PrinterPort: 9100, Columns: 48, PaperWidthMM: 80,
			CutPaperSeparately: true, PrintBoutique: true, Active: true,
		},
	}
	for _, st := range defaultStations {
		cutInt, wInt, dInt, bInt, btInt, fInt := 0, 0, 0, 0, 0, 0
		if st.CutPaperSeparately {
			cutInt = 1
		}
		if st.PrintWaiterCalls {
			wInt = 1
		}
		if st.PrintDrinks {
			dInt = 1
		}
		if st.PrintBillClosure {
			bInt = 1
		}
		if st.PrintBoutique {
			btInt = 1
		}
		if st.PrintFiscalNotes {
			fInt = 1
		}
		_, _ = s.DB.Exec(`INSERT OR IGNORE INTO printer_stations (
			id, name, sector_label, connection_type, pc_name, windows_printer_name,
			printer_ip, printer_port, columns, paper_width_mm,
			cut_paper_separately, print_waiter_calls, print_drinks, print_bill_closure, print_boutique, print_fiscal_notes, active
		) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
			st.ID, st.Name, st.SectorLabel, st.ConnectionType, st.PCName,
			st.PrinterIP, st.PrinterPort, st.Columns, st.PaperWidthMM,
			cutInt, wInt, dInt, bInt, btInt, fInt)
		// Atualiza metadados padrão em bancos antigos onde sector_label ainda estava vazio
		_, _ = s.DB.Exec(`UPDATE printer_stations SET
			name = ?, sector_label = ?, connection_type = ?, pc_name = ?,
			cut_paper_separately = ?, print_waiter_calls = ?, print_drinks = ?, print_bill_closure = ?, print_boutique = ?, print_fiscal_notes = ?
			WHERE id = ? AND TRIM(COALESCE(sector_label, '')) = ''`,
			st.Name, st.SectorLabel, st.ConnectionType, st.PCName,
			cutInt, wInt, dInt, bInt, btInt, fInt, st.ID)
	}
	// Garante que a categoria de Porções aponte por padrão para COZINHA_PORCOES para sair em papel cortado separado do Lanche!
	_, _ = s.DB.Exec(`UPDATE category_printer_routes SET printer_station_id = 'COZINHA_PORCOES'
		WHERE (LOWER(category_name) LIKE '%porç%' OR LOWER(category_name) LIKE '%porc%')
		  AND printer_station_id = 'COZINHA_CHAPA'`)

	// Garante o turno inicial aberto da operadora de Caixa principal (Amanda - ID 2) apenas se ela ainda existir em users
	var amandaCount int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE id = 2`).Scan(&amandaCount)
	isoOpen := time.Now().Add(-3 * time.Hour).Format("2006-01-02 15:04:05")
	if amandaCount > 0 {
		_, _ = s.DB.Exec(`INSERT OR IGNORE INTO cashier_shifts (cashier_id, cashier_name, cashier_username, status, opening_float, opened_at, opened_at_iso, opened_by_admin)
			VALUES (2, 'Amanda (Operadora PDV & Mercado)', 'caixa', 'ABERTO', 150.0, ?, ?, 'Guilherme (Diretor / ADM)')`, time.Now().Format("02/01 18:00"), isoOpen)
	}
	_, _ = s.DB.Exec(`DELETE FROM cashier_shifts WHERE cashier_id NOT IN (SELECT id FROM users)`)
	_, _ = s.DB.Exec(`DELETE FROM motoboy_shifts WHERE motoboy_id NOT IN (SELECT id FROM users)`)
	_, _ = s.DB.Exec(`UPDATE cashier_shifts SET opened_at_iso = ? WHERE opened_at_iso = '' AND status = 'ABERTO'`, isoOpen)
	_, _ = s.DB.Exec(`UPDATE orders SET created_at_iso = ? WHERE created_at_iso = ''`, time.Now().Add(-1*time.Hour).Format("2006-01-02 15:04:05"))

	// Garante pelo menos 1 pedido em RETIRADA NO BALCÃO para o Operador de Caixa visualizar e receber igual às mesas
	var retCount int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM orders WHERE UPPER(origin_type) = 'RETIRADA' OR UPPER(header_tag) LIKE '%RETIRADA%'`).Scan(&retCount)
	if retCount == 0 {
		retOrder := models.Order{
			OrderCode:       "SB-1045",
			OriginType:      "RETIRADA",
			HeaderTag:       "[RETIRADA BALCÃO]",
			CustomerName:    "Juliana Martins",
			CustomerPhone:   "5511977776666",
			AddressText:     "RETIRADA NO BALCÃO SIR BEEF",
			Latitude:        -23.55052,
			Longitude:       -46.63331,
			Status:          "PRONTO",
			PaymentMethod:   "PIX",
			StatusPagamento: "PENDENTE",
			Subtotal:        89.00,
			DeliveryFee:     0.00,
			Total:           89.00,
			Items: []models.OrderItem{
				{
					ProductID:   2,
					ProductName: "Picanha",
					Quantity:    2,
					UnitPrice:   36.00,
					TotalPrice:  72.00,
					SectorABC:   "HAMBURGUERIA",
					SelectedModifiers: []models.SelectedModifier{
						{GroupName: "Ponto da Carne", OptionName: "Ao Ponto (Recomendado Sir Beef)", Price: 0},
					},
				},
				{
					ProductID:   36,
					ProductName: "Coca-Cola 2L",
					Quantity:    1,
					UnitPrice:   17.00,
					TotalPrice:  17.00,
					SectorABC:   "MERCADO_BEBIDAS",
				},
			},
		}
		_, _ = s.CreateOrder(retOrder)
	}

	return err
}

func (s *Store) seedIfEmpty(seedJSONPath string) error {
	var count int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
	if count > 0 {
		return nil
	}

	now := time.Now().Format("15:04")
	fullNow := time.Now().Format("02/01/2006 15:04:05")

	// 1. Seed Users (RBAC para os 4 cargos do Super App Kotlin)
	users := []models.User{
		{Name: "Guilherme (Diretor / ADM)", Username: "admin", PIN: "1111", Role: "ADMIN", Active: true, Status: "APROVADO"},
		{Name: "Amanda (Operadora PDV & Mercado)", Username: "caixa", PIN: "2222", Role: "CAIXA", Active: true, Status: "APROVADO"},
		{Name: "Lucas (Garçom Salão & Deck)", Username: "garcom", PIN: "3333", Role: "GARCOM", Active: true, Status: "APROVADO"},
		{Name: "Rafael (Motoboy Express 01)", Username: "motoboy", PIN: "4444", Role: "MOTOBOY", Active: true, Status: "APROVADO"},
		{Name: "Diego (Motoboy Express 02)", Username: "diego", PIN: "5555", Role: "MOTOBOY", Active: true, Status: "APROVADO"},
		{Name: "Carlos Eduardo (Novo Funcionário)", Username: "carlos", PIN: "1234", Role: "GARCOM", Active: false, Status: "PENDENTE"},
	}
	for _, u := range users {
		act := 1
		if !u.Active {
			act = 0
		}
		_, _ = s.DB.Exec(`INSERT INTO users (name, username, pin, role, active, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			u.Name, u.Username, u.PIN, u.Role, act, u.Status, fullNow)
	}

	// 2. Seed Printer Config (ESC/POS Dinâmico)
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO printer_config (id, columns, paper_width_mm, printer_ip, printer_port, auto_print_on_accept, whatsapp_bot_phase, gemini_api_key)
		VALUES (1, 48, 80, '192.168.1.200', 9100, 1, 1, '')`)

	// 3. Seed Catalog (Importado do cardápio real + Boutique das Carnes)
	if data, err := os.ReadFile(seedJSONPath); err == nil {
		var cats []models.Category
		if err := json.Unmarshal(data, &cats); err == nil {
			for _, c := range cats {
				sector := c.Sector
				if sector == "" {
					sector = "HAMBURGUERIA"
				}
				sectorABC := c.SectorABC
				if sectorABC == "" {
					sectorABC = "HAMBURGUERIA"
				}
				_, _ = s.DB.Exec(`INSERT OR REPLACE INTO categories (id, name, sector, sector_abc) VALUES (?, ?, ?, ?)`,
					c.ID, c.Name, sector, sectorABC)

				for _, it := range c.Items {
					modJSON, _ := json.Marshal(it.ModifierGroups)
					weightedInt := 0
					if it.IsWeighted {
						weightedInt = 1
					}
					price := it.Price
					if price <= 0 && len(it.ModifierGroups) > 0 && len(it.ModifierGroups[0].Options) > 0 {
						price = it.ModifierGroups[0].Options[0].Price
					}
					_, _ = s.DB.Exec(`INSERT INTO products (
						external_id, category_id, category_name, sector, sector_abc, name, description,
						price, promo_from, cost_price, ean13, plu_code, is_weighted, ncm, cest, cfop,
						image_url, modifier_groups_json, active
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
						it.ExternalID, c.ID, c.Name, sector, sectorABC, it.Name, it.Description,
						price, it.PromoFrom, it.CostPrice, it.EAN13, it.PLUCode, weightedInt,
						it.NCM, it.CEST, it.CFOP, it.ImageURL, string(modJSON),
					)
				}
			}
		}
	}

	// 4. Seed Mesas do Salão + Mesas Avulsas ([Q] e [G])
	for i := 1; i <= 12; i++ {
		status := "LIVRE"
		total := 0.0
		if i == 4 {
			status = "OCUPADA"
			total = 86.00
		} else if i == 12 {
			status = "CONTA"
			total = 112.00
		}
		label := fmt.Sprintf("MESA %02d", i)
		token := fmt.Sprintf("SIRBEEF-MESA-%02d", i)
		_, _ = s.DB.Exec(`INSERT INTO dining_tables (number, label, origin_prefix, is_custom, status, qr_token, current_total, updated_at)
			VALUES (?, ?, '[Q]', 0, ?, ?, ?, ?)`, i, label, status, token, total, now)
	}
	// Mesa avulsa aberta pelo Garçom ([G] BISTRÔ CALÇADA)
	_, _ = s.DB.Exec(`INSERT INTO dining_tables (number, label, origin_prefix, is_custom, status, qr_token, current_total, updated_at)
		VALUES (99, 'BISTRÔ CALÇADA', '[G]', 1, 'OCUPADA', 'SIRBEEF-GARCOM-99', 74.00, ?)`, now)

	// 5. Seed Pedidos de Exemplo no Kanban e Salão para demonstração imediata
	sampleOrders := []models.Order{
		{
			OrderCode:       "SB-1041",
			OriginType:      "DELIVERY",
			HeaderTag:       "[DELIVERY]",
			CustomerName:    "Marcos Vinícius",
			CustomerPhone:   "5511998765432",
			AddressText:     "Av. Paulista, 1578 - Apto 82 (Bela Vista)",
			Latitude:        -23.56141,
			Longitude:       -46.65588,
			Status:          "RECEBIDO",
			PaymentMethod:   "DINHEIRO",
			StatusPagamento: "PENDENTE",
			ChangeFor:       100.00,
			Subtotal:        72.00,
			DeliveryFee:     8.00,
			Total:           80.00,
			Items: []models.OrderItem{
				{
					ProductID:   2,
					ProductName: "Picanha",
					Quantity:    2,
					UnitPrice:   36.00,
					TotalPrice:  72.00,
					SectorABC:   "HAMBURGUERIA",
					SelectedModifiers: []models.SelectedModifier{
						{GroupName: "Tipo Burger", OptionName: "Burger Simples", Price: 0},
						{GroupName: "Ponto da Carne", OptionName: "Ao Ponto (Recomendado Sir Beef)", Price: 0},
						{GroupName: "Retiradas", OptionName: "Sem Cebola", Price: 0},
					},
					Notes: "Capricha no bacon crocante!",
				},
			},
		},
		{
			OrderCode:       "SB-1042",
			OriginType:      "DELIVERY",
			HeaderTag:       "[DELIVERY]",
			CustomerName:    "Fernanda Costa",
			CustomerPhone:   "5511991234567",
			AddressText:     "Rua Augusta, 2210 - Jardins",
			Latitude:        -23.56412,
			Longitude:       -46.66231,
			Status:          "PRONTO",
			PaymentMethod:   "DINHEIRO",
			StatusPagamento: "PENDENTE",
			ChangeFor:       100.00,
			Subtotal:        69.00,
			DeliveryFee:     9.00,
			Total:           78.00,
			Items: []models.OrderItem{
				{
					ProductID:   4,
					ProductName: "Costela Especial",
					Quantity:    1,
					UnitPrice:   39.00,
					TotalPrice:  39.00,
					SectorABC:   "HAMBURGUERIA",
					SelectedModifiers: []models.SelectedModifier{
						{GroupName: "Tipo Burger", OptionName: "Burger Duplo", Price: 3.00},
						{GroupName: "Ponto da Carne", OptionName: "Mal Passado", Price: 0},
					},
				},
				{
					ProductID:   30,
					ProductName: "Batata Grande",
					Quantity:    1,
					UnitPrice:   30.00,
					TotalPrice:  30.00,
					SectorABC:   "HAMBURGUERIA",
				},
			},
		},
		{
			OrderCode:       "SB-1043",
			OriginType:      "MESA",
			HeaderTag:       "MESA 12",
			CustomerName:    "Mesa 12 (Autoatendimento QR)",
			CustomerPhone:   "5511988887777",
			AddressText:     "Salão Principal - Mesa 12",
			Latitude:        -23.55052,
			Longitude:       -46.63331,
			Status:          "PREPARANDO",
			PaymentMethod:   "CARTAO_ENTREGA",
			StatusPagamento: "PENDENTE",
			Subtotal:        112.00,
			DeliveryFee:     0.00,
			Total:           112.00,
			Items: []models.OrderItem{
				{
					ProductID:   9,
					ProductName: "Angus Rodeio",
					Quantity:    2,
					UnitPrice:   42.00,
					TotalPrice:  84.00,
					SectorABC:   "HAMBURGUERIA",
					SelectedModifiers: []models.SelectedModifier{
						{GroupName: "Ponto da Carne", OptionName: "Ao Ponto (Recomendado Sir Beef)", Price: 0},
					},
				},
				{
					ProductID:   36,
					ProductName: "Coca-Cola",
					Quantity:    2,
					UnitPrice:   14.00,
					TotalPrice:  28.00,
					SectorABC:   "MERCADO_BEBIDAS",
				},
			},
		},
		{
			OrderCode:       "SB-1044",
			OriginType:      "PDV_MERCADO",
			HeaderTag:       "[PDV MERCADO]",
			CustomerName:    "Cliente Boutique Balcão",
			CustomerPhone:   "",
			AddressText:     "Balcão Boutique das Carnes",
			Latitude:        -23.55052,
			Longitude:       -46.63331,
			Status:          "ENTREGUE",
			PaymentMethod:   "PIX_ENTREGA",
			StatusPagamento: "PAGO",
			Subtotal:        199.60,
			DeliveryFee:     0.00,
			Total:           199.60,
			Items: []models.OrderItem{
				{
					ProductID:   52,
					ProductName: "Picanha Angus Gold Sir Beef (Kg)",
					Quantity:    1.25,
					UnitPrice:   119.90,
					TotalPrice:  149.80,
					SectorABC:   "MERCADO_CARNES",
				},
				{
					ProductID:   55,
					ProductName: "Sal de Parrilla & Chimichurri Reserva 500g",
					Quantity:    2,
					UnitPrice:   24.90,
					TotalPrice:  49.80,
					SectorABC:   "MERCADO_CARNES",
				},
			},
		},
	}

	for _, o := range sampleOrders {
		_, _ = s.CreateOrder(o)
	}

	// 6. Seed System Logs (Central de Debug)
	_, _ = s.DB.Exec(`INSERT INTO system_logs (level, module, message, stack_trace, device_info, created_at) VALUES
		('INFO', 'HOST-GO', 'Servidor Host Local Sir Beef iniciado em modo SQLite WAL + Failover pronto', 'main.go:88 -> db.OpenStore(WAL_ENABLED)', 'Host Windows 11 x64 (Go 1.27)', ?),
		('WARN', 'PRINTER-ESCPOS', 'Impressora térmica de rede 192.168.1.200:9100 em modo spooler virtual (sem resposta de hardware físico na LAN)', 'internal/escpos/escpos.go:135 -> net.DialTimeout("tcp", "192.168.1.200:9100", 2500ms)\n  at escpos.SendToNetworkPrinter(escpos.go:138)\n  at server.handlePrintOrder(server.go:240)', 'Spooler ESC/POS 48 Colunas (80mm)', ?),
		('CRASH_KOTLIN', 'KOTLIN-SUPERAPP', 'Timeout no IP Primário LAN (192.168.15.99:8080) — Failover acionado automaticamente para Túnel Cloudflare', 'java.net.SocketTimeoutException: failed to connect to /192.168.15.99 (port 8080) after 800ms\n  at okhttp3.internal.connection.RealConnection.connectSocket(RealConnection.kt:297)\n  at br.com.sirbeef.superapp.network.SmartHostInterceptor.intercept(SmartHostInterceptor.kt:42)', 'Samsung SM-S918B (Android 14 - Role: MOTOBOY)', ?)
	`, fullNow, fullNow, fullNow)

	return nil
}

func (s *Store) Authenticate(pinOrUser string) (*models.User, error) {
	u, status, err := s.AuthenticateCredentials(pinOrUser, "")
	if err != nil {
		return nil, err
	}
	if status != "APROVADO" {
		return nil, fmt.Errorf("acesso %s", status)
	}
	return u, nil
}

func (s *Store) AuthenticateCredentials(usernameOrName string, password string) (*models.User, string, error) {
	usernameOrName = strings.TrimSpace(usernameOrName)
	password = strings.TrimSpace(password)
	if usernameOrName == "" && password != "" {
		usernameOrName = password
		password = ""
	}

	users, err := s.ListUsers()
	if err != nil {
		return nil, "ERROR", err
	}

	var matched *models.User
	qLower := strings.ToLower(usernameOrName)
	for i := range users {
		u := &users[i]
		uNameLower := strings.ToLower(strings.TrimSpace(u.Name))
		uUserLower := strings.ToLower(strings.TrimSpace(u.Username))
		uRoleLower := strings.ToLower(strings.TrimSpace(u.Role))
		// Permite logar pelo username exato, pelo nome completo, pelo primeiro nome, pelo cargo/apelido (adm) ou pelo PIN rápido
		firstWord := strings.Fields(uNameLower)
		firstMatch := len(firstWord) > 0 && firstWord[0] == qLower
		nameContains := qLower != "" && strings.Contains(uNameLower, qLower)
		roleMatch := qLower == uRoleLower || (qLower == "adm" && uRoleLower == "admin")

		if password != "" {
			if (uUserLower == qLower || uNameLower == qLower || firstMatch || nameContains || roleMatch || u.PIN == qLower) && u.PIN == password {
				matched = u
				break
			}
		} else {
			if u.PIN == usernameOrName || uUserLower == qLower || uNameLower == qLower || firstMatch {
				matched = u
				break
			}
		}
	}
	// Fallback: se digitou apenas o PIN na senha (ou usuário+senha iguais ao PIN)
	if matched == nil && password != "" {
		for i := range users {
			u := &users[i]
			if u.PIN == password && (usernameOrName == "" || usernameOrName == password) {
				matched = u
				break
			}
		}
	}

	if matched == nil {
		return nil, "INVALID", fmt.Errorf("Nome/usuário ou senha incorretos.")
	}

	if matched.Status == "PENDENTE" || (!matched.Active && matched.Status != "BLOQUEADO") {
		return matched, "PENDENTE", fmt.Errorf("⏳ Cadastro aguardando aprovação do ADM! Solicite ao Administrador liberar seu acesso (%s).", matched.Role)
	}
	if !matched.Active || matched.Status == "BLOQUEADO" {
		return matched, "BLOQUEADO", fmt.Errorf("🚫 Seu acesso foi bloqueado ou recusado pelo Administrador.")
	}
	// Gera um novo session_token único a cada login aprovado (sessão única por usuário: derruba qualquer login anterior na mesma conta)
	newSessionToken := fmt.Sprintf("sess-%d-%d", matched.ID, time.Now().UnixNano())
	matched.SessionToken = newSessionToken
	_, _ = s.DB.Exec(`UPDATE users SET session_token = ? WHERE id = ?`, newSessionToken, matched.ID)
	_ = s.AddSystemLog("INFO", "AUTH-SESSAO-UNICA", fmt.Sprintf("🔑 Login aprovado para %s (@%s | %s) -> Nova sessão ativa: %s (sessões anteriores neste login foram desconectadas)", matched.Name, matched.Username, matched.Role, newSessionToken), "", "AUTH-LOGIN")
	return matched, "APROVADO", nil
}

func (s *Store) RegisterEmployee(name, username, password, role string) (*models.User, error) {
	name = strings.TrimSpace(name)
	username = strings.ToLower(strings.TrimSpace(username))
	password = strings.TrimSpace(password)
	role = strings.ToUpper(strings.TrimSpace(role))

	if name == "" {
		return nil, fmt.Errorf("Informe seu nome para o cadastro.")
	}
	if password == "" {
		return nil, fmt.Errorf("Crie uma senha para o seu login.")
	}
	if username == "" {
		parts := strings.Fields(strings.ToLower(name))
		if len(parts) > 0 {
			username = parts[0]
		} else {
			username = "func"
		}
	}
	if role != "ADMIN" && role != "CAIXA" && role != "GARCOM" && role != "MOTOBOY" && role != "TABLET" {
		role = "GARCOM"
	}

	// Garante username único caso já exista homônimo
	baseUser := username
	suffix := 1
	for {
		var exists int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE LOWER(username) = LOWER(?)`, username).Scan(&exists)
		if exists == 0 {
			break
		}
		suffix++
		username = fmt.Sprintf("%s%d", baseUser, suffix)
	}

	nowStr := time.Now().Format("02/01/2006 15:04")
	res, err := s.DB.Exec(`INSERT INTO users (name, username, pin, role, active, status, created_at) VALUES (?, ?, ?, ?, 0, 'PENDENTE', ?)`,
		name, username, password, role, nowStr)
	if err != nil {
		return nil, err
	}
	lastID, _ := res.LastInsertId()

	_, _ = s.DB.Exec(`INSERT INTO system_logs (level, module, message, stack_trace, device_info, created_at) VALUES ('INFO', 'RBAC-CADASTRO', ?, ?, 'Super App Android Kotlin', ?)`,
		fmt.Sprintf("Novo funcionário solicitou acesso: %s (login: %s | cargo solicitado: %s)", name, username, role),
		fmt.Sprintf("Status=PENDENTE | ID=%d | Aguardando aceite do ADMIN", lastID),
		nowStr,
	)

	return &models.User{
		ID:        int(lastID),
		Name:      name,
		Username:  username,
		PIN:       password,
		Role:      role,
		Active:    false,
		Status:    "PENDENTE",
		CreatedAt: nowStr,
	}, nil
}

func extractTableNumberFromText(s string) int {
	digits := ""
	for _, ch := range s {
		if ch >= '0' && ch <= '9' {
			digits += string(ch)
		}
	}
	if n, err := strconv.Atoi(digits); err == nil && n > 0 && n <= 99 {
		return n
	}
	return 0
}

func (s *Store) ListUsers() ([]models.User, error) {
	rows, err := s.DB.Query(`SELECT id, name, username, pin, role, COALESCE(table_number, 0), active, COALESCE(status, 'APROVADO'), COALESCE(created_at, ''), COALESCE(session_token, '') FROM users ORDER BY CASE WHEN status = 'PENDENTE' THEN 0 WHEN role = 'TABLET' THEN 2 ELSE 1 END, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := []models.User{}
	for rows.Next() {
		var u models.User
		var active int
		if err := rows.Scan(&u.ID, &u.Name, &u.Username, &u.PIN, &u.Role, &u.TableNumber, &active, &u.Status, &u.CreatedAt, &u.SessionToken); err == nil {
			u.Active = active == 1
			if u.Status == "" {
				if u.Active {
					u.Status = "APROVADO"
				} else {
					u.Status = "PENDENTE"
				}
			}
			if u.Role == "TABLET" && u.TableNumber == 0 {
				u.TableNumber = extractTableNumberFromText(u.Username)
				if u.TableNumber == 0 {
					u.TableNumber = extractTableNumberFromText(u.Name)
				}
			}
			res = append(res, u)
		}
	}
	return res, nil
}

func (s *Store) GetActiveSessionTokens() map[string]string {
	res := map[string]string{}
	rows, err := s.DB.Query(`SELECT id, LOWER(TRIM(COALESCE(username, ''))), COALESCE(session_token, '') FROM users WHERE TRIM(COALESCE(session_token, '')) != ''`)
	if err != nil {
		return res
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var uname, tok string
		if rows.Scan(&id, &uname, &tok) == nil && tok != "" {
			res[strconv.Itoa(id)] = tok
			if uname != "" {
				res[uname] = tok
			}
		}
	}
	return res
}

func (s *Store) CreateOrUpdateUser(u models.User) error {
	u.Role = strings.ToUpper(strings.TrimSpace(u.Role))
	if u.Role != "ADMIN" && u.Role != "CAIXA" && u.Role != "GARCOM" && u.Role != "MOTOBOY" && u.Role != "TABLET" {
		u.Role = "GARCOM"
	}
	u.Name = strings.TrimSpace(u.Name)
	u.Username = strings.ToLower(strings.TrimSpace(u.Username))
	u.PIN = strings.TrimSpace(u.PIN)

	if u.Role == "TABLET" {
		if u.TableNumber <= 0 {
			u.TableNumber = extractTableNumberFromText(u.Username)
			if u.TableNumber <= 0 {
				u.TableNumber = extractTableNumberFromText(u.Name)
			}
			if u.TableNumber <= 0 {
				u.TableNumber = 1
			}
		}
		if u.Username == "" {
			u.Username = fmt.Sprintf("mesa%02d", u.TableNumber)
		}
		if u.Name == "" {
			u.Name = fmt.Sprintf("Tablet Kiosk — Mesa %02d", u.TableNumber)
		}
		if u.PIN == "" && u.ID == 0 {
			u.PIN = "1234"
		}
	}

	if u.Status == "" {
		if u.Active || u.ID == 0 {
			u.Status = "APROVADO"
			u.Active = true
		} else {
			u.Status = "PENDENTE"
		}
	}
	if u.Status == "APROVADO" {
		u.Active = true
	} else if u.Status == "PENDENTE" || u.Status == "BLOQUEADO" {
		u.Active = false
	}
	active := 0
	if u.Active {
		active = 1
	}

	if u.ID == 0 && u.Username != "" {
		var existingID int
		_ = s.DB.QueryRow(`SELECT id FROM users WHERE LOWER(username) = LOWER(?) LIMIT 1`, u.Username).Scan(&existingID)
		if existingID > 0 {
			u.ID = existingID
		}
	}

	if u.ID > 0 {
		var cur models.User
		var curAct int
		err := s.DB.QueryRow(`SELECT id, name, username, pin, role, COALESCE(table_number,0), active, COALESCE(status,'APROVADO'), COALESCE(created_at,'') FROM users WHERE id = ?`, u.ID).
			Scan(&cur.ID, &cur.Name, &cur.Username, &cur.PIN, &cur.Role, &cur.TableNumber, &curAct, &cur.Status, &cur.CreatedAt)
		if err == nil {
			if strings.TrimSpace(u.Name) == "" {
				u.Name = cur.Name
			}
			if strings.TrimSpace(u.Username) == "" {
				u.Username = cur.Username
			}
			if strings.TrimSpace(u.PIN) == "" {
				u.PIN = cur.PIN
			}
			if u.TableNumber <= 0 {
				u.TableNumber = cur.TableNumber
			}
		}
		_, err = s.DB.Exec(`UPDATE users SET name=?, username=?, pin=?, role=?, table_number=?, active=?, status=? WHERE id=?`,
			u.Name, u.Username, u.PIN, u.Role, u.TableNumber, active, u.Status, u.ID)
		return err
	}
	if u.PIN == "" {
		u.PIN = "1234"
	}
	if u.Username == "" {
		parts := strings.Fields(strings.ToLower(u.Name))
		if len(parts) > 0 {
			u.Username = parts[0]
		} else {
			u.Username = fmt.Sprintf("user%d", time.Now().Unix()%1000)
		}
	}
	nowStr := time.Now().Format("02/01/2006 15:04")
	_, err := s.DB.Exec(`INSERT INTO users (name, username, pin, role, table_number, active, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Name, u.Username, u.PIN, u.Role, u.TableNumber, active, u.Status, nowStr)
	return err
}

func (s *Store) DeleteUser(id int) error {
	if id <= 0 {
		return fmt.Errorf("id de usuário inválido")
	}
	var uname, urole string
	_ = s.DB.QueryRow(`SELECT LOWER(COALESCE(username,'')), UPPER(COALESCE(role,'')) FROM users WHERE id = ?`, id).Scan(&uname, &urole)
	if id == 1 || uname == "admin" {
		return fmt.Errorf("🚫 Não é permitido excluir a conta principal do Administrador (ADM).")
	}
	if urole == "ADMIN" {
		var adminCount int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE UPPER(role) = 'ADMIN' AND active = 1`).Scan(&adminCount)
		if adminCount <= 1 {
			return fmt.Errorf("🚫 Não é possível excluir o único Administrador (ADM) ativo do sistema.")
		}
	}
	_, err := s.DB.Exec(`DELETE FROM users WHERE id = ?`, id)
	_, _ = s.DB.Exec(`DELETE FROM cashier_shifts WHERE cashier_id = ?`, id)
	_, _ = s.DB.Exec(`DELETE FROM motoboy_shifts WHERE motoboy_id = ?`, id)
	return err
}

func (s *Store) activeTodayWeeklyPromoMap() map[int]models.WeeklyPromotion {
	today := int(time.Now().Weekday()) // 0=Domingo .. 6=Sábado
	rows, err := s.DB.Query(`SELECT id, day_of_week, product_id, original_price, promo_price, active, COALESCE(updated_at,'')
		FROM weekly_promotions
		WHERE active = 1 AND (day_of_week = ? OR day_of_week = 7)
		ORDER BY id DESC`, today)
	res := map[int]models.WeeklyPromotion{}
	if err != nil {
		return res
	}
	defer rows.Close()
	for rows.Next() {
		var wp models.WeeklyPromotion
		var actInt int
		if err := rows.Scan(&wp.ID, &wp.DayOfWeek, &wp.ProductID, &wp.OriginalPrice, &wp.PromoPrice, &actInt, &wp.UpdatedAt); err == nil {
			wp.Active = actInt == 1
			if _, exists := res[wp.ProductID]; !exists && wp.PromoPrice > 0 {
				res[wp.ProductID] = wp
			}
		}
	}
	return res
}

func normalizeModifierOptionsVisibility(opts []models.ModifierOption) []models.ModifierOption {
	if opts == nil {
		return []models.ModifierOption{}
	}
	for i := range opts {
		if opts[i].Disabled {
			f := false
			opts[i].Visible = &f
		} else if opts[i].Visible == nil {
			t := true
			opts[i].Visible = &t
		} else if !*opts[i].Visible {
			opts[i].Disabled = true
		} else {
			opts[i].Disabled = false
		}
	}
	return opts
}

func IsVariationModifierGroup(g models.ModifierGroup) bool {
	if len(g.Options) == 0 {
		return false
	}
	gLow := strings.ToLower(strings.TrimSpace(g.Name))
	isNamedVar := strings.Contains(gLow, "varia") || strings.Contains(gLow, "tipo") ||
		strings.Contains(gLow, "tamanho") || strings.Contains(gLow, "porção") || strings.Contains(gLow, "porcao") ||
		strings.Contains(gLow, "sabor") || strings.Contains(gLow, "com gás") || strings.Contains(gLow, "com gas") ||
		strings.Contains(gLow, "versão") || strings.Contains(gLow, "versao") || strings.Contains(gLow, "opção principal")

	visibleCount := 0
	positiveCount := 0
	for _, opt := range g.Options {
		if opt.Disabled || (opt.Visible != nil && !*opt.Visible) {
			continue
		}
		visibleCount++
		if opt.Price > 0 {
			positiveCount++
		}
	}
	if visibleCount == 0 {
		return false
	}
	if isNamedVar && g.Max <= 1 && positiveCount > 0 {
		return true
	}
	if !g.Required && g.Min < 1 {
		return false
	}
	return g.Max <= 1 && positiveCount == visibleCount
}

func ComputeMinVariationPrice(groups []models.ModifierGroup) (float64, bool) {
	for _, g := range groups {
		if !IsVariationModifierGroup(g) {
			continue
		}
		minP := 0.0
		found := false
		for _, opt := range g.Options {
			if opt.Disabled || (opt.Visible != nil && !*opt.Visible) {
				continue
			}
			if opt.Price > 0 && (!found || opt.Price < minP) {
				minP = opt.Price
				found = true
			}
		}
		if found && minP > 0 {
			return minP, true
		}
	}
	return 0, false
}

func normalizeProductPromoAndModifiers(p *models.Product, activePromoMap map[int]models.WeeklyPromotion) {
	// 1. Nunca deixa "Ponto da Carne" em Porções, Batatas, Onion Rings, Lanches de Frango, Marmitas, Bebidas ou Sobremesas
	catLower := strings.ToLower(p.CategoryName)
	nameLower := strings.ToLower(p.Name)
	isNonBeef := strings.Contains(catLower, "porç") || strings.Contains(catLower, "porc") ||
		strings.Contains(catLower, "bebida") || strings.Contains(catLower, "marmita") ||
		strings.Contains(catLower, "sobremesa") || strings.Contains(nameLower, "batata") ||
		strings.Contains(nameLower, "chicken") || strings.Contains(nameLower, "frango") ||
		strings.Contains(nameLower, "onion") || strings.Contains(nameLower, "kitkat")
	if isNonBeef && len(p.ModifierGroups) > 0 {
		filtered := make([]models.ModifierGroup, 0, len(p.ModifierGroups))
		for _, g := range p.ModifierGroups {
			if !strings.Contains(strings.ToLower(g.Name), "ponto") {
				filtered = append(filtered, g)
			}
		}
		p.ModifierGroups = filtered
	}
	for gi := range p.ModifierGroups {
		p.ModifierGroups[gi].Options = normalizeModifierOptionsVisibility(p.ModifierGroups[gi].Options)
		if IsVariationModifierGroup(p.ModifierGroups[gi]) {
			p.ModifierGroups[gi].Required = true
			p.ModifierGroups[gi].Min = 1
			p.ModifierGroups[gi].Max = 1
		}
	}
	// 2. Se o produto tiver Variação (ex: Burger Simples R$ 36 / Burger Duplo R$ 50), o preço base "A partir de" é sempre o MENOR valor da variação!
	if minVarPrice, hasVar := ComputeMinVariationPrice(p.ModifierGroups); hasVar && minVarPrice > 0 {
		p.Price = minVarPrice
	}
	// 3. Garante que promo_from (preço riscado) seja sempre MAIOR que price (preço de oferta ativo)
	if p.PromoFrom != nil && *p.PromoFrom > 0 && *p.PromoFrom < p.Price {
		oldP := p.Price
		newP := *p.PromoFrom
		p.Price = newP
		p.PromoFrom = &oldP
	}
	// 4. Se houver Promoção da Semana ativa para hoje neste produto, aplica automaticamente!
	if wp, ok := activePromoMap[p.ID]; ok && wp.PromoPrice > 0 {
		orig := p.Price
		if wp.OriginalPrice > orig {
			orig = wp.OriginalPrice
		}
		if p.PromoFrom != nil && *p.PromoFrom > orig {
			orig = *p.PromoFrom
		}
		p.Price = wp.PromoPrice
		if orig > p.Price {
			p.PromoFrom = &orig
		}
	}
}

// deduplicateCategories remove categorias duplicadas (como IDs antigos 100..106 sem itens) e unifica produtos pelo nome da categoria.
func (s *Store) deduplicateCategories() {
	// Remove categorias antigas 100..106 que estejam vazias se já existir a categoria real (134..176)
	_, _ = s.DB.Exec(`DELETE FROM categories WHERE id BETWEEN 100 AND 106 AND id NOT IN (SELECT DISTINCT category_id FROM products)`)
	_, _ = s.DB.Exec(`DELETE FROM category_printer_routes WHERE category_id NOT IN (SELECT id FROM categories)`)

	rows, err := s.DB.Query(`
		SELECT c.id, c.name, c.sector, c.sector_abc,
		       (SELECT COUNT(*) FROM products p WHERE p.category_id = c.id) AS prod_count
		FROM categories c
		ORDER BY prod_count DESC, c.id DESC
	`)
	if err != nil {
		return
	}
	type catRow struct {
		id        int
		name      string
		sector    string
		sectorABC string
		count     int
	}
	var all []catRow
	for rows.Next() {
		var r catRow
		if rows.Scan(&r.id, &r.name, &r.sector, &r.sectorABC, &r.count) == nil {
			all = append(all, r)
		}
	}
	rows.Close()

	canonicalByName := map[string]catRow{}
	for _, r := range all {
		norm := strings.ToLower(strings.TrimSpace(r.name))
		if norm == "" {
			continue
		}
		if keep, exists := canonicalByName[norm]; !exists {
			canonicalByName[norm] = r
		} else {
			// Move eventuais produtos da categoria duplicada para a categoria principal e exclui a duplicada
			_, _ = s.DB.Exec(`UPDATE products SET category_id = ?, category_name = ? WHERE category_id = ?`, keep.id, keep.name, r.id)
			_, _ = s.DB.Exec(`DELETE FROM categories WHERE id = ?`, r.id)
			_, _ = s.DB.Exec(`DELETE FROM category_printer_routes WHERE category_id = ?`, r.id)
		}
	}
	// Sincroniza nomes de categorias na tabela products
	_, _ = s.DB.Exec(`UPDATE products SET category_name = (SELECT name FROM categories WHERE categories.id = products.category_id) WHERE category_id IN (SELECT id FROM categories)`)
}

// SaveCategory cria uma nova categoria ou edita/renomeia uma categoria existente, sincronizando produtos e rotas de impressora.
func (s *Store) SaveCategory(id int, name string, sector string, printerStationID string) (*models.Category, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("informe o nome da categoria")
	}
	sector = strings.ToUpper(strings.TrimSpace(sector))
	if sector != "MERCADO" {
		sector = "HAMBURGUERIA"
	}
	sectorABC := "HAMBURGUERIA"
	if sector == "MERCADO" {
		sectorABC = "MERCADO_CARNES"
	}
	printerStationID = strings.ToUpper(strings.TrimSpace(printerStationID))

	if id > 0 {
		var exists int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM categories WHERE id = ?`, id).Scan(&exists)
		if exists > 0 {
			_, err := s.DB.Exec(`UPDATE categories SET name = ?, sector = ?, sector_abc = ? WHERE id = ?`, name, sector, sectorABC, id)
			if err != nil {
				return nil, err
			}
			_, _ = s.DB.Exec(`UPDATE products SET category_name = ?, sector = ?, sector_abc = ? WHERE category_id = ?`, name, sector, sectorABC, id)
			_, _ = s.DB.Exec(`UPDATE category_printer_routes SET category_name = ? WHERE category_id = ?`, name, id)
			if printerStationID != "" {
				_ = s.SaveCategoryPrinterRoute(id, printerStationID)
			}
			_ = s.AddSystemLog("INFO", "CATEGORIAS-CRUD", fmt.Sprintf("Categoria #%d atualizada para '%s' (%s)", id, name, sector), "", "ADM")
			return &models.Category{ID: id, Name: name, Sector: sector, SectorABC: sectorABC}, nil
		}
	}

	// Verifica se já existe categoria com o mesmo nome para não duplicar
	var existingID int
	_ = s.DB.QueryRow(`SELECT id FROM categories WHERE LOWER(TRIM(name)) = LOWER(TRIM(?)) LIMIT 1`, name).Scan(&existingID)
	if existingID > 0 {
		return s.SaveCategory(existingID, name, sector, printerStationID)
	}

	var maxID int
	_ = s.DB.QueryRow(`SELECT COALESCE(MAX(id), 176) FROM categories WHERE id < 900`).Scan(&maxID)
	if maxID < 180 {
		maxID = 180
	} else {
		maxID++
	}
	newID := maxID
	if id > 0 {
		newID = id
	}

	_, err := s.DB.Exec(`INSERT INTO categories (id, name, sector, sector_abc) VALUES (?, ?, ?, ?)`, newID, name, sector, sectorABC)
	if err != nil {
		return nil, err
	}
	if printerStationID == "" {
		printerStationID = defaultStationForCategory(name, sector)
	}
	_ = s.SaveCategoryPrinterRoute(newID, printerStationID)
	_ = s.AddSystemLog("INFO", "CATEGORIAS-CRUD", fmt.Sprintf("Nova categoria criada: '%s' (ID #%d | Setor Impressão: %s)", name, newID, printerStationID), "", "ADM")
	return &models.Category{ID: newID, Name: name, Sector: sector, SectorABC: sectorABC}, nil
}

// DeleteCategory exclui uma categoria do cardápio. Se houver produtos nela, move-os para moveToCategoryID (ou exclui os produtos se moveToCategoryID == -999).
func (s *Store) DeleteCategory(id int, moveToCategoryID int) error {
	if id <= 0 {
		return fmt.Errorf("ID de categoria inválido")
	}
	var catName string
	err := s.DB.QueryRow(`SELECT name FROM categories WHERE id = ?`, id).Scan(&catName)
	if err != nil {
		return fmt.Errorf("categoria #%d não encontrada", id)
	}

	var prodCount int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM products WHERE category_id = ?`, id).Scan(&prodCount)
	if prodCount > 0 {
		if moveToCategoryID == -999 {
			_, _ = s.DB.Exec(`DELETE FROM products WHERE category_id = ?`, id)
		} else {
			targetID := moveToCategoryID
			var targetName, targetSector, targetABC string
			if targetID > 0 && targetID != id {
				_ = s.DB.QueryRow(`SELECT name, sector, sector_abc FROM categories WHERE id = ?`, targetID).Scan(&targetName, &targetSector, &targetABC)
			}
			if targetName == "" {
				_ = s.DB.QueryRow(`SELECT id, name, sector, sector_abc FROM categories WHERE id != ? ORDER BY CASE WHEN id = 134 THEN 0 ELSE 1 END, id ASC LIMIT 1`, id).
					Scan(&targetID, &targetName, &targetSector, &targetABC)
			}
			if targetID > 0 && targetName != "" {
				_, _ = s.DB.Exec(`UPDATE products SET category_id = ?, category_name = ?, sector = ?, sector_abc = ? WHERE category_id = ?`,
					targetID, targetName, targetSector, targetABC, id)
			}
		}
	}

	_, err = s.DB.Exec(`DELETE FROM categories WHERE id = ?`, id)
	_, _ = s.DB.Exec(`DELETE FROM category_printer_routes WHERE category_id = ?`, id)
	_ = s.AddSystemLog("WARN", "CATEGORIAS-CRUD", fmt.Sprintf("Categoria '%s' (#%d) excluída pelo ADM (%d produtos realocados/removidos).", catName, id, prodCount), "", "ADM")
	return err
}

func (s *Store) GetCatalog() ([]models.Category, error) {
	return s.GetCatalogAll(false)
}

func (s *Store) GetCatalogAll(includeInactive bool) ([]models.Category, error) {
	catRows, err := s.DB.Query(`SELECT id, name, sector, sector_abc FROM categories ORDER BY CASE WHEN id = 137 THEN 0 ELSE 1 END, id ASC`)
	if err != nil {
		return nil, err
	}
	defer catRows.Close()

	var cats []models.Category
	catMap := map[int]int{}
	for catRows.Next() {
		var c models.Category
		if err := catRows.Scan(&c.ID, &c.Name, &c.Sector, &c.SectorABC); err == nil {
			c.Items = []models.Product{}
			catMap[c.ID] = len(cats)
			cats = append(cats, c)
		}
	}

	activePromoMap := s.activeTodayWeeklyPromoMap()

	query := `SELECT id, external_id, category_id, category_name, sector, sector_abc, name, description,
		price, promo_from, cost_price, ean13, plu_code, is_weighted, ncm, cest, cfop, image_url, modifier_groups_json,
		COALESCE(stock_qty, 40.0), COALESCE(min_stock, 5.0), COALESCE(track_stock, 1), active
		FROM products`
	if !includeInactive {
		query += ` WHERE active = 1`
	}
	query += ` ORDER BY id ASC`

	prodRows, err := s.DB.Query(query)
	if err != nil {
		return cats, nil
	}
	defer prodRows.Close()

	ofertaIdx, hasOfertaCat := catMap[137]
	existingInOferta := map[int]bool{}

	for prodRows.Next() {
		var p models.Product
		var promo sql.NullFloat64
		var weighted, trackStock, active int
		var modJSON string
		err := prodRows.Scan(
			&p.ID, &p.ExternalID, &p.CategoryID, &p.CategoryName, &p.Sector, &p.SectorABC,
			&p.Name, &p.Description, &p.Price, &promo, &p.CostPrice, &p.EAN13, &p.PLUCode,
			&weighted, &p.NCM, &p.CEST, &p.CFOP, &p.ImageURL, &modJSON,
			&p.StockQty, &p.MinStock, &trackStock, &active,
		)
		if err != nil {
			continue
		}
		if promo.Valid && promo.Float64 > 0 {
			val := promo.Float64
			p.PromoFrom = &val
		}
		p.IsWeighted = weighted == 1
		p.TrackStock = trackStock == 1
		p.Active = active == 1
		_ = json.Unmarshal([]byte(modJSON), &p.ModifierGroups)
		if p.ModifierGroups == nil {
			p.ModifierGroups = []models.ModifierGroup{}
		}
		normalizeProductPromoAndModifiers(&p, activePromoMap)

		if idx, ok := catMap[p.CategoryID]; ok {
			cats[idx].Items = append(cats[idx].Items, p)
			if p.CategoryID == 137 {
				existingInOferta[p.ID] = true
			}
		} else if len(cats) > 0 {
			cats[0].Items = append(cats[0].Items, p)
		}

		// Se o produto está na Promoção da Semana de hoje e ainda não está na categoria "Oferta do dia", destaca-o lá também!
		if !includeInactive && hasOfertaCat && p.CategoryID != 137 {
			if _, isScheduledToday := activePromoMap[p.ID]; isScheduledToday && !existingInOferta[p.ID] {
				existingInOferta[p.ID] = true
				cats[ofertaIdx].Items = append([]models.Product{p}, cats[ofertaIdx].Items...)
			}
		}
	}
	return cats, nil
}

func (s *Store) ListAllProducts() ([]models.Product, error) {
	activePromoMap := s.activeTodayWeeklyPromoMap()
	rows, err := s.DB.Query(`SELECT id, external_id, category_id, category_name, sector, sector_abc, name, description,
		price, promo_from, cost_price, ean13, plu_code, is_weighted, ncm, cest, cfop, image_url, modifier_groups_json,
		COALESCE(stock_qty, 40.0), COALESCE(min_stock, 5.0), COALESCE(track_stock, 1), active
		FROM products ORDER BY sector ASC, category_id ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []models.Product
	for rows.Next() {
		var p models.Product
		var promo sql.NullFloat64
		var weighted, trackStock, active int
		var modJSON string
		if err := rows.Scan(
			&p.ID, &p.ExternalID, &p.CategoryID, &p.CategoryName, &p.Sector, &p.SectorABC,
			&p.Name, &p.Description, &p.Price, &promo, &p.CostPrice, &p.EAN13, &p.PLUCode,
			&weighted, &p.NCM, &p.CEST, &p.CFOP, &p.ImageURL, &modJSON,
			&p.StockQty, &p.MinStock, &trackStock, &active,
		); err == nil {
			if promo.Valid && promo.Float64 > 0 {
				val := promo.Float64
				p.PromoFrom = &val
			}
			p.IsWeighted = weighted == 1
			p.TrackStock = trackStock == 1
			p.Active = active == 1
			_ = json.Unmarshal([]byte(modJSON), &p.ModifierGroups)
			if p.ModifierGroups == nil {
				p.ModifierGroups = []models.ModifierGroup{}
			}
			normalizeProductPromoAndModifiers(&p, activePromoMap)
			list = append(list, p)
		}
	}
	return list, nil
}

// LookupBarcode suporta tanto códigos EAN-13 padrão quanto etiquetas de balança (Toledo/Filizola 13 dígitos começando com "2").
func (s *Store) LookupBarcode(code string) (*models.Product, float64, error) {
	code = strings.TrimSpace(code)
	// Verifica etiqueta de balança padrão 2 + 4 dígitos PLU + 5 dígitos peso (gramas) ou valor + dígito verificador
	if len(code) == 13 && strings.HasPrefix(code, "2") {
		plu := code[1:5]
		weightGramsStr := code[7:12]
		var p models.Product
		var weighted, trackStock, active int
		var modJSON string
		err := s.DB.QueryRow(`SELECT id, external_id, category_id, category_name, sector, sector_abc, name, description,
			price, cost_price, ean13, plu_code, is_weighted, ncm, cest, cfop, image_url, modifier_groups_json,
			COALESCE(stock_qty, 40.0), COALESCE(min_stock, 5.0), COALESCE(track_stock, 1), active
			FROM products WHERE (plu_code = ? OR SUBSTR(ean13, 1, 5) = ?) AND active = 1 LIMIT 1`,
			plu, code[0:5]).Scan(
			&p.ID, &p.ExternalID, &p.CategoryID, &p.CategoryName, &p.Sector, &p.SectorABC,
			&p.Name, &p.Description, &p.Price, &p.CostPrice, &p.EAN13, &p.PLUCode,
			&weighted, &p.NCM, &p.CEST, &p.CFOP, &p.ImageURL, &modJSON,
			&p.StockQty, &p.MinStock, &trackStock, &active,
		)
		if err == nil {
			p.IsWeighted = weighted == 1
			p.TrackStock = trackStock == 1
			p.Active = active == 1
			_ = json.Unmarshal([]byte(modJSON), &p.ModifierGroups)
			grams, _ := strconv.ParseFloat(weightGramsStr, 64)
			qtyKg := grams / 1000.0
			if qtyKg <= 0 {
				qtyKg = 1.0
			}
			return &p, qtyKg, nil
		}
	}

	var p models.Product
	var weighted, trackStock, active int
	var modJSON string
	err := s.DB.QueryRow(`SELECT id, external_id, category_id, category_name, sector, sector_abc, name, description,
		price, cost_price, ean13, plu_code, is_weighted, ncm, cest, cfop, image_url, modifier_groups_json,
		COALESCE(stock_qty, 40.0), COALESCE(min_stock, 5.0), COALESCE(track_stock, 1), active
		FROM products WHERE (ean13 = ? OR CAST(id AS TEXT) = ? OR LOWER(name) LIKE LOWER(?)) AND active = 1 LIMIT 1`,
		code, code, "%"+code+"%").Scan(
		&p.ID, &p.ExternalID, &p.CategoryID, &p.CategoryName, &p.Sector, &p.SectorABC,
		&p.Name, &p.Description, &p.Price, &p.CostPrice, &p.EAN13, &p.PLUCode,
		&weighted, &p.NCM, &p.CEST, &p.CFOP, &p.ImageURL, &modJSON,
		&p.StockQty, &p.MinStock, &trackStock, &active,
	)
	if err != nil {
		return nil, 0, err
	}
	p.IsWeighted = weighted == 1
	p.TrackStock = trackStock == 1
	p.Active = active == 1
	_ = json.Unmarshal([]byte(modJSON), &p.ModifierGroups)
	return &p, 1.0, nil
}

func (s *Store) CreateOrUpdateProduct(p models.Product) (*models.Product, error) {
	if p.CategoryName != "" {
		var realID int
		var realName string
		_ = s.DB.QueryRow(`SELECT id, name FROM categories WHERE LOWER(TRIM(name)) = LOWER(TRIM(?)) LIMIT 1`, p.CategoryName).Scan(&realID, &realName)
		if realID > 0 {
			p.CategoryID = realID
			p.CategoryName = realName
		}
	}
	if p.CategoryID == 0 || (p.CategoryID >= 100 && p.CategoryID <= 106) {
		if strings.EqualFold(p.Sector, "MERCADO") {
			p.CategoryID = 900
			p.CategoryName = "Boutique das Carnes & Empório"
		} else {
			p.CategoryID = 134
			p.CategoryName = "Burgers Gourmet"
		}
	}
	if p.CategoryName == "" {
		_ = s.DB.QueryRow(`SELECT name FROM categories WHERE id = ?`, p.CategoryID).Scan(&p.CategoryName)
		if p.CategoryName == "" {
			p.CategoryName = "Burgers Gourmet"
		}
	}
	if p.Sector == "" {
		if p.CategoryID == 900 {
			p.Sector = "MERCADO"
		} else {
			p.Sector = "HAMBURGUERIA"
		}
	}
	if p.SectorABC == "" {
		if p.Sector == "MERCADO" {
			p.SectorABC = "MERCADO_CARNES"
		} else {
			p.SectorABC = "HAMBURGUERIA"
		}
	}
	if p.NCM == "" {
		p.NCM = "21069090"
	}
	if p.CEST == "" {
		p.CEST = "1709900"
	}
	if p.CFOP == "" {
		p.CFOP = "5102"
	}
	if p.MinStock <= 0 {
		p.MinStock = 5.0
	}

	// Se o produto possui grupo de variação (ex: Burger Simples / Burger Duplo), sincroniza automaticamente o preço base com o menor valor da variação!
	for gi := range p.ModifierGroups {
		p.ModifierGroups[gi].Options = normalizeModifierOptionsVisibility(p.ModifierGroups[gi].Options)
		if IsVariationModifierGroup(p.ModifierGroups[gi]) {
			p.ModifierGroups[gi].Required = true
			p.ModifierGroups[gi].Min = 1
			p.ModifierGroups[gi].Max = 1
		}
	}
	if minVarPrice, hasVar := ComputeMinVariationPrice(p.ModifierGroups); hasVar && minVarPrice > 0 {
		p.Price = minVarPrice
	}

	// Normaliza oferta: se promo_from foi preenchido menor que price, inverte automaticamente para que promo_from seja sempre o preço antigo (maior/riscado) e price o preço promocional (menor)
	var promoVal interface{} = nil
	if p.PromoFrom != nil && *p.PromoFrom > 0 {
		pf := *p.PromoFrom
		if pf < p.Price && p.Price > 0 {
			oldP := p.Price
			p.Price = pf
			pf = oldP
			p.PromoFrom = &pf
		}
		if pf > p.Price {
			promoVal = pf
		} else {
			p.PromoFrom = nil
		}
	}

	if p.CostPrice <= 0 && p.Price > 0 {
		p.CostPrice = math.Round(p.Price*0.45*100) / 100
	}

	// Se o usuário digitou -1 (ou negativo), significa ESTOQUE INFINITO (-1)
	if p.StockQty < 0 {
		p.StockQty = -1
		p.Active = true
	}

	// Garante que a categoria exista
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO categories (id, name, sector, sector_abc) VALUES (?, ?, ?, ?)`,
		p.CategoryID, p.CategoryName, p.Sector, p.SectorABC)

	weighted := 0
	if p.IsWeighted {
		weighted = 1
	}
	trackStock := 1
	if !p.TrackStock {
		trackStock = 0
	}
	active := 1
	if !p.Active && p.StockQty >= 0 {
		active = 0
	}

	if p.ID > 0 {
		var existingModJSON, existingImg string
		_ = s.DB.QueryRow(`SELECT modifier_groups_json, image_url FROM products WHERE id = ?`, p.ID).Scan(&existingModJSON, &existingImg)
		modJSONStr := existingModJSON
		if p.ModifierGroups != nil {
			b, _ := json.Marshal(p.ModifierGroups)
			modJSONStr = string(b)
		}
		if modJSONStr == "" {
			modJSONStr = "[]"
		}
		if strings.EqualFold(strings.TrimSpace(p.ImageURL), "NONE") || strings.EqualFold(strings.TrimSpace(p.ImageURL), "SEM_FOTO") {
			p.ImageURL = ""
		} else if p.ImageURL == "" && !strings.EqualFold(p.Sector, "MERCADO") {
			p.ImageURL = existingImg
		}

		_, err := s.DB.Exec(`UPDATE products SET
			category_id = ?, category_name = ?, sector = ?, sector_abc = ?,
			name = ?, description = ?, price = ?, promo_from = ?, cost_price = ?,
			ean13 = ?, plu_code = ?, is_weighted = ?, ncm = ?, cest = ?, cfop = ?,
			image_url = ?, modifier_groups_json = ?, stock_qty = ?, min_stock = ?, track_stock = ?, active = ?
			WHERE id = ?`,
			p.CategoryID, p.CategoryName, p.Sector, p.SectorABC,
			p.Name, p.Description, p.Price, promoVal, p.CostPrice,
			p.EAN13, p.PLUCode, weighted, p.NCM, p.CEST, p.CFOP,
			p.ImageURL, modJSONStr, p.StockQty, p.MinStock, trackStock, active, p.ID,
		)
		if err != nil {
			return nil, err
		}
		return &p, nil
	}

	if strings.EqualFold(strings.TrimSpace(p.ImageURL), "NONE") || strings.EqualFold(strings.TrimSpace(p.ImageURL), "SEM_FOTO") {
		p.ImageURL = ""
	}
	if p.StockQty == 0 {
		p.StockQty = 30.0
	}
	p.Active = true
	p.TrackStock = true
	modJSON, _ := json.Marshal(p.ModifierGroups)
	if len(p.ModifierGroups) == 0 {
		modJSON = []byte("[]")
	}

	res, err := s.DB.Exec(`INSERT INTO products (
		external_id, category_id, category_name, sector, sector_abc, name, description,
		price, promo_from, cost_price, ean13, plu_code, is_weighted, ncm, cest, cfop, image_url, modifier_groups_json,
		stock_qty, min_stock, track_stock, active
	) VALUES (0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 1)`,
		p.CategoryID, p.CategoryName, p.Sector, p.SectorABC, p.Name, p.Description,
		p.Price, promoVal, p.CostPrice, p.EAN13, p.PLUCode, weighted, p.NCM, p.CEST, p.CFOP, p.ImageURL, string(modJSON),
		p.StockQty, p.MinStock,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	p.ID = int(id)
	return &p, nil
}

func (s *Store) CreateProduct(p models.Product) (*models.Product, error) {
	return s.CreateOrUpdateProduct(p)
}

func (s *Store) SetProductImageURL(id int, imageURL string) error {
	img := strings.TrimSpace(imageURL)
	if strings.EqualFold(img, "NONE") || strings.EqualFold(img, "SEM_FOTO") {
		img = ""
	}
	_, err := s.DB.Exec(`UPDATE products SET image_url = ? WHERE id = ?`, img, id)
	return err
}

func (s *Store) QuickUpdateProduct(id int, action string, value float64) error {
	act := strings.ToUpper(strings.TrimSpace(action))
	switch act {
	case "TOGGLE_ACTIVE":
		_, err := s.DB.Exec(`UPDATE products SET active = CASE WHEN active = 1 THEN 0 ELSE 1 END WHERE id = ?`, id)
		return err
	case "SET_ACTIVE":
		v := 0
		if value > 0 {
			v = 1
		}
		_, err := s.DB.Exec(`UPDATE products SET active = ? WHERE id = ?`, v, id)
		return err
	case "SET_PRICE":
		if value < 0 {
			value = 0
		}
		_, err := s.DB.Exec(`UPDATE products SET price = ? WHERE id = ?`, value, id)
		return err
	case "SET_PROMO_FROM":
		if value <= 0 {
			_, err := s.DB.Exec(`UPDATE products SET promo_from = NULL WHERE id = ?`, id)
			return err
		}
		_, err := s.DB.Exec(`UPDATE products SET promo_from = ? WHERE id = ?`, value, id)
		_, _ = s.DB.Exec(`UPDATE products SET price = promo_from, promo_from = price WHERE id = ? AND promo_from IS NOT NULL AND promo_from > 0 AND promo_from < price`, id)
		return err
	case "SET_COST":
		if value < 0 {
			value = 0
		}
		_, err := s.DB.Exec(`UPDATE products SET cost_price = ? WHERE id = ?`, value, id)
		return err
	case "SET_STOCK":
		if value < 0 {
			_, err := s.DB.Exec(`UPDATE products SET stock_qty = -1, active = 1 WHERE id = ?`, id)
			return err
		}
		active := 1
		if value <= 0 {
			active = 0
		}
		_, err := s.DB.Exec(`UPDATE products SET stock_qty = ?, active = ? WHERE id = ?`, value, active, id)
		return err
	case "TOGGLE_INFINITE_STOCK":
		var curStock float64
		_ = s.DB.QueryRow(`SELECT COALESCE(stock_qty, 40) FROM products WHERE id = ?`, id).Scan(&curStock)
		if curStock < 0 {
			fallback := value
			if fallback <= 0 {
				fallback = 40
			}
			_, err := s.DB.Exec(`UPDATE products SET stock_qty = ?, active = 1 WHERE id = ?`, fallback, id)
			return err
		}
		_, err := s.DB.Exec(`UPDATE products SET stock_qty = -1, active = 1 WHERE id = ?`, id)
		return err
	case "CLEAR_IMAGE":
		_, err := s.DB.Exec(`UPDATE products SET image_url = '' WHERE id = ?`, id)
		return err
	case "ADJUST_STOCK":
		_, err := s.DB.Exec(`UPDATE products SET
			stock_qty = CASE WHEN COALESCE(stock_qty, 0) < 0 THEN -1 ELSE MAX(0, COALESCE(stock_qty, 0) + ?) END,
			active = CASE WHEN COALESCE(stock_qty, 0) < 0 THEN 1 WHEN (COALESCE(stock_qty, 0) + ?) <= 0 THEN 0 ELSE 1 END
			WHERE id = ?`, value, value, id)
		return err
	}
	return nil
}

func (s *Store) ensureDefaultModifierGroupTemplatesAndCleanPorcoes() {
	// Produtos do Mercado / Boutique ficam sem foto por padrão (apenas escrito), a menos que o usuário suba uma foto própria
	_, _ = s.DB.Exec(`UPDATE products SET image_url = '' WHERE (sector = 'MERCADO' OR category_id = 900) AND image_url LIKE '/assets/products/item_900%'`)

	// 1. Limpa "Ponto da Carne" de todas as Porções, Batatas, Onion Rings, Frango/Chicken, Marmitas, Bebidas e Sobremesas já salvas no banco
	rows, err := s.DB.Query(`SELECT id, name, category_name, modifier_groups_json FROM products`)
	if err == nil {
		type prodUpdate struct {
			id      int
			modJSON string
		}
		var updates []prodUpdate
		for rows.Next() {
			var pid int
			var pname, cname, mjson string
			if err := rows.Scan(&pid, &pname, &cname, &mjson); err == nil {
				catLower := strings.ToLower(cname)
				nameLower := strings.ToLower(pname)
				isNonBeef := strings.Contains(catLower, "porç") || strings.Contains(catLower, "porc") ||
					strings.Contains(catLower, "bebida") || strings.Contains(catLower, "marmita") ||
					strings.Contains(catLower, "sobremesa") || strings.Contains(nameLower, "batata") ||
					strings.Contains(nameLower, "chicken") || strings.Contains(nameLower, "frango") ||
					strings.Contains(nameLower, "onion") || strings.Contains(nameLower, "kitkat")
				if isNonBeef && strings.Contains(strings.ToLower(mjson), "ponto") {
					var groups []models.ModifierGroup
					if json.Unmarshal([]byte(mjson), &groups) == nil {
						var clean []models.ModifierGroup
						for _, g := range groups {
							if !strings.Contains(strings.ToLower(g.Name), "ponto") {
								clean = append(clean, g)
							}
						}
						if clean == nil {
							clean = []models.ModifierGroup{}
						}
						if b, err := json.Marshal(clean); err == nil {
							updates = append(updates, prodUpdate{id: pid, modJSON: string(b)})
						}
					}
				}
			}
		}
		rows.Close()
		for _, u := range updates {
			_, _ = s.DB.Exec(`UPDATE products SET modifier_groups_json = ? WHERE id = ?`, u.modJSON, u.id)
		}
	}

	// 2. Se modifier_group_templates estiver vazia, cria os Grupos de Adicionais padrão para Lanches e Porções
	var count int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM modifier_group_templates`).Scan(&count)
	if count == 0 {
		defaults := []models.ModifierGroup{
			{
				Name:     "Ponto da Carne",
				Min:      1,
				Max:      1,
				Required: true,
				Options: []models.ModifierOption{
					{Name: "Ao Ponto (Recomendado Sir Beef)", Description: "Rosado e suculento no centro", Price: 0},
					{Name: "Mal Passado", Description: "Selado por fora, vermelho e quente no centro", Price: 0},
					{Name: "Bem Passado", Description: "Totalmente assado", Price: 0},
				},
			},
			{
				Name:     "Adicionais de Lanches",
				Min:      0,
				Max:      40,
				Required: false,
				Options: []models.ModifierOption{
					{Name: "COMBO (Refri + Batata)", Price: 10.0},
					{Name: "COMBO (Suco + Batata)", Price: 14.0},
					{Name: "Bacon", Price: 5.0},
					{Name: "Catupiry", Price: 5.0},
					{Name: "Catupiry Original empanado", Price: 8.0},
					{Name: "Queijo Cheddar", Price: 2.0},
					{Name: "Queijo mussarela", Price: 3.0},
					{Name: "Ovo", Price: 2.0},
					{Name: "Presunto", Price: 2.0},
					{Name: "Cebola caramelizada", Price: 2.0},
					{Name: "Cebola roxa", Price: 1.0},
					{Name: "Rúcula", Price: 1.0},
					{Name: "Picles", Price: 1.0},
					{Name: "Onion Rings", Price: 3.0},
					{Name: "Abacaxi Caramelizado c/ Sweet Chilli", Price: 3.0},
					{Name: "Sweet Chilli Goiabada", Price: 2.0},
					{Name: "Molho Barbecue", Price: 2.0},
					{Name: "Molho Especial", Price: 1.0},
					{Name: "Partir lanche ao meio", Price: 1.0},
					{Name: "3 Sachês Extra de Maionese", Price: 1.0},
					{Name: "3 Sachês Extra de Katchup", Price: 1.0},
					{Name: "3 Sachês Extra de Mostarda", Price: 1.0},
				},
			},
			{
				Name:     "Molhos à parte (potinho)",
				Min:      0,
				Max:      5,
				Required: false,
				Options: []models.ModifierOption{
					{Name: "Maionese de Bacon", Price: 3.0},
					{Name: "Maionese Verde", Price: 3.0},
					{Name: "Maionese de Alho", Price: 3.0},
					{Name: "Molho Especial", Price: 3.0},
					{Name: "Molho Barbecue", Price: 3.0},
				},
			},
			{
				Name:     "Retiradas (Sem custo)",
				Min:      0,
				Max:      6,
				Required: false,
				Options: []models.ModifierOption{
					{Name: "Sem Alface", Price: 0},
					{Name: "Sem Tomate", Price: 0},
					{Name: "Sem Cebola", Price: 0},
					{Name: "Sem Bacon", Price: 0},
					{Name: "Sem Queijo", Price: 0},
					{Name: "Sem Molho", Price: 0},
				},
			},
			{
				Name:     "Adicionais de Batata / Porções",
				Min:      0,
				Max:      3,
				Required: false,
				Options: []models.ModifierOption{
					{Name: "Bacon", Price: 10.0},
					{Name: "Catupiry Batata", Price: 10.0},
					{Name: "Cheddar Cremoso", Price: 10.0},
				},
			},
		}
		for _, g := range defaults {
			optJSON, _ := json.Marshal(g.Options)
			reqInt := 0
			if g.Required {
				reqInt = 1
			}
			_, _ = s.DB.Exec(`INSERT INTO modifier_group_templates (name, min_select, max_select, required, options_json) VALUES (?, ?, ?, ?, ?)`,
				g.Name, g.Min, g.Max, reqInt, string(optJSON))
		}
	}
}

func (s *Store) ListModifierGroupTemplates() ([]models.ModifierGroup, error) {
	rows, err := s.DB.Query(`SELECT id, name, min_select, max_select, required, options_json FROM modifier_group_templates ORDER BY id ASC`)
	if err != nil {
		return []models.ModifierGroup{}, err
	}
	defer rows.Close()
	var list []models.ModifierGroup
	for rows.Next() {
		var g models.ModifierGroup
		var reqInt int
		var optJSON string
		if err := rows.Scan(&g.ID, &g.Name, &g.Min, &g.Max, &reqInt, &optJSON); err == nil {
			g.Required = reqInt == 1
			_ = json.Unmarshal([]byte(optJSON), &g.Options)
			g.Options = normalizeModifierOptionsVisibility(g.Options)
			list = append(list, g)
		}
	}
	if list == nil {
		list = []models.ModifierGroup{}
	}
	return list, nil
}

// SaveModifierGroupTemplate cria ou edita um Grupo de Adicionais reutilizável e propaga automaticamente
// qualquer mudança nas opções/preços para todos os produtos que já utilizam esse grupo!
func (s *Store) SaveModifierGroupTemplate(g models.ModifierGroup) (*models.ModifierGroup, error) {
	g.Name = strings.TrimSpace(g.Name)
	if g.Name == "" {
		return nil, fmt.Errorf("informe o nome do Grupo de Adicionais")
	}
	if g.Max <= 0 {
		g.Max = 1
	}
	if g.Min < 0 {
		g.Min = 0
	}
	g.Options = normalizeModifierOptionsVisibility(g.Options)
	optJSON, _ := json.Marshal(g.Options)
	reqInt := 0
	if g.Required {
		reqInt = 1
	}

	var oldName string
	if g.ID > 0 {
		_ = s.DB.QueryRow(`SELECT name FROM modifier_group_templates WHERE id = ?`, g.ID).Scan(&oldName)
		_, err := s.DB.Exec(`UPDATE modifier_group_templates SET name = ?, min_select = ?, max_select = ?, required = ?, options_json = ? WHERE id = ?`,
			g.Name, g.Min, g.Max, reqInt, string(optJSON), g.ID)
		if err != nil {
			return nil, err
		}
	} else {
		res, err := s.DB.Exec(`INSERT INTO modifier_group_templates (name, min_select, max_select, required, options_json) VALUES (?, ?, ?, ?, ?)`,
			g.Name, g.Min, g.Max, reqInt, string(optJSON))
		if err != nil {
			return nil, err
		}
		lastID, _ := res.LastInsertId()
		g.ID = int(lastID)
	}

	// Propaga automaticamente as alterações deste grupo para os produtos que já o utilizam
	s.syncModifierGroupTemplateToExistingProducts(g, oldName)
	_ = s.AddSystemLog("INFO", "GRUPOS-ADICIONAIS", fmt.Sprintf("Grupo de Adicionais '%s' (ID #%d, %d opções) salvo e sincronizado nos produtos.", g.Name, g.ID, len(g.Options)), "", "ADM")
	return &g, nil
}

// ToggleModifierGroupOptionVisibility alterna com 1 clique se uma opção específica dentro de um Grupo de Adicionais está Visível (em estoque) ou Invisível (esgotado em todos os lanches).
func (s *Store) ToggleModifierGroupOptionVisibility(groupID int, groupName string, optIndex int, optName string, visible bool) (*models.ModifierGroup, error) {
	templates, err := s.ListModifierGroupTemplates()
	if err != nil {
		return nil, err
	}
	var target *models.ModifierGroup
	for i := range templates {
		if (groupID > 0 && templates[i].ID == groupID) || (groupName != "" && strings.EqualFold(strings.TrimSpace(templates[i].Name), strings.TrimSpace(groupName))) {
			target = &templates[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("grupo de adicionais não encontrado")
	}

	matchedIdx := -1
	cleanOptName := strings.TrimSpace(optName)
	if optIndex >= 0 && optIndex < len(target.Options) {
		if cleanOptName == "" || strings.EqualFold(strings.TrimSpace(target.Options[optIndex].Name), cleanOptName) {
			matchedIdx = optIndex
		}
	}
	if matchedIdx == -1 && cleanOptName != "" {
		for idx := range target.Options {
			if strings.EqualFold(strings.TrimSpace(target.Options[idx].Name), cleanOptName) {
				matchedIdx = idx
				break
			}
		}
	}
	if matchedIdx == -1 {
		return nil, fmt.Errorf("adicional '%s' não encontrado no grupo '%s'", optName, target.Name)
	}

	vis := visible
	target.Options[matchedIdx].Visible = &vis
	target.Options[matchedIdx].Disabled = !visible

	saved, err := s.SaveModifierGroupTemplate(*target)
	if err != nil {
		return nil, err
	}
	stateStr := "VISÍVEL"
	if !visible {
		stateStr = "INVISÍVEL (Esgotado)"
	}
	_ = s.AddSystemLog("INFO", "ADICIONAL-VISIBILIDADE", fmt.Sprintf("Adicional '%s' no grupo '%s' marcado como %s em todos os lanches.", target.Options[matchedIdx].Name, target.Name, stateStr), "", "ADM")
	return saved, nil
}

func (s *Store) syncModifierGroupTemplateToExistingProducts(g models.ModifierGroup, oldName string) {
	rows, err := s.DB.Query(`SELECT id, category_name, name, modifier_groups_json FROM products`)
	if err != nil {
		return
	}
	type prodSync struct {
		id      int
		modJSON string
	}
	var toUpdate []prodSync
	gNameLower := strings.ToLower(strings.TrimSpace(g.Name))
	oldNameLower := strings.ToLower(strings.TrimSpace(oldName))
	isBurgerExtrasGroup := strings.Contains(gNameLower, "adicionais de lanches") || gNameLower == "adicionais"
	isPorcaoExtrasGroup := strings.Contains(gNameLower, "batata") || strings.Contains(gNameLower, "porç") || strings.Contains(gNameLower, "porc")

	for rows.Next() {
		var pid int
		var cname, pname, mjson string
		if err := rows.Scan(&pid, &cname, &pname, &mjson); err != nil {
			continue
		}
		var groups []models.ModifierGroup
		if json.Unmarshal([]byte(mjson), &groups) != nil || len(groups) == 0 {
			continue
		}
		catLower := strings.ToLower(cname)
		isPorcaoProd := strings.Contains(catLower, "porç") || strings.Contains(catLower, "porc") || strings.Contains(strings.ToLower(pname), "batata")
		changed := false
		for i := range groups {
			curNameLower := strings.ToLower(strings.TrimSpace(groups[i].Name))
			match := (g.ID > 0 && groups[i].ID == g.ID) ||
				curNameLower == gNameLower ||
				(oldNameLower != "" && curNameLower == oldNameLower) ||
				(isBurgerExtrasGroup && !isPorcaoProd && (curNameLower == "adicionais" || curNameLower == "adicionais de lanches")) ||
				(isPorcaoExtrasGroup && isPorcaoProd && (curNameLower == "adicionais" || strings.Contains(curNameLower, "batata")))
			if match {
				groups[i] = models.ModifierGroup{
					ID:       g.ID,
					Name:     g.Name,
					Min:      g.Min,
					Max:      g.Max,
					Required: g.Required,
					Options:  g.Options,
				}
				changed = true
			}
		}
		if changed {
			if b, err := json.Marshal(groups); err == nil {
				toUpdate = append(toUpdate, prodSync{id: pid, modJSON: string(b)})
			}
		}
	}
	rows.Close()
	for _, u := range toUpdate {
		_, _ = s.DB.Exec(`UPDATE products SET modifier_groups_json = ? WHERE id = ?`, u.modJSON, u.id)
	}
}

func (s *Store) DeleteModifierGroupTemplate(id int) error {
	_, err := s.DB.Exec(`DELETE FROM modifier_group_templates WHERE id = ?`, id)
	return err
}

// ApplyModifierGroupTemplateToCategory aplica um Grupo de Adicionais inteiro a todos os produtos de uma Categoria (ou lista de IDs) com 1 clique.
func (s *Store) ApplyModifierGroupTemplateToCategory(groupID int, categoryID int) (int, error) {
	templates, err := s.ListModifierGroupTemplates()
	if err != nil {
		return 0, err
	}
	var target *models.ModifierGroup
	for i := range templates {
		if templates[i].ID == groupID {
			target = &templates[i]
			break
		}
	}
	if target == nil {
		return 0, fmt.Errorf("grupo de adicionais não encontrado")
	}

	query := `SELECT id, modifier_groups_json FROM products`
	var args []interface{}
	if categoryID > 0 {
		query += ` WHERE category_id = ?`
		args = append(args, categoryID)
	}
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return 0, err
	}
	type itemUpd struct {
		id      int
		modJSON string
	}
	var updates []itemUpd
	for rows.Next() {
		var pid int
		var mjson string
		if err := rows.Scan(&pid, &mjson); err == nil {
			var groups []models.ModifierGroup
			_ = json.Unmarshal([]byte(mjson), &groups)
			found := false
			for i := range groups {
				if groups[i].ID == target.ID || strings.EqualFold(strings.TrimSpace(groups[i].Name), strings.TrimSpace(target.Name)) {
					groups[i] = *target
					found = true
					break
				}
			}
			if !found {
				groups = append(groups, *target)
			}
			if b, err := json.Marshal(groups); err == nil {
				updates = append(updates, itemUpd{id: pid, modJSON: string(b)})
			}
		}
	}
	rows.Close()
	for _, u := range updates {
		_, _ = s.DB.Exec(`UPDATE products SET modifier_groups_json = ? WHERE id = ?`, u.modJSON, u.id)
	}
	return len(updates), nil
}

func weekdayLabelPT(day int) string {
	switch day {
	case 0:
		return "Domingo"
	case 1:
		return "Segunda-feira"
	case 2:
		return "Terça-feira"
	case 3:
		return "Quarta-feira"
	case 4:
		return "Quinta-feira"
	case 5:
		return "Sexta-feira"
	case 6:
		return "Sábado"
	case 7:
		return "Todos os Dias"
	default:
		return "Dia da Semana"
	}
}

func (s *Store) ListWeeklyPromotions() ([]models.WeeklyPromotion, error) {
	rows, err := s.DB.Query(`SELECT wp.id, wp.day_of_week, wp.product_id, COALESCE(p.name, 'Produto Removido'), COALESCE(p.category_name, ''),
		CASE WHEN wp.original_price > 0 THEN wp.original_price ELSE COALESCE(p.price, 0) END,
		wp.promo_price, wp.active, COALESCE(wp.updated_at, '')
		FROM weekly_promotions wp
		LEFT JOIN products p ON p.id = wp.product_id
		ORDER BY CASE WHEN wp.day_of_week = 0 THEN 7 ELSE wp.day_of_week END ASC, wp.id ASC`)
	if err != nil {
		return []models.WeeklyPromotion{}, err
	}
	defer rows.Close()
	var list []models.WeeklyPromotion
	for rows.Next() {
		var wp models.WeeklyPromotion
		var actInt int
		if err := rows.Scan(&wp.ID, &wp.DayOfWeek, &wp.ProductID, &wp.ProductName, &wp.CategoryName, &wp.OriginalPrice, &wp.PromoPrice, &actInt, &wp.UpdatedAt); err == nil {
			wp.Active = actInt == 1
			wp.DayName = weekdayLabelPT(wp.DayOfWeek)
			list = append(list, wp)
		}
	}
	if list == nil {
		list = []models.WeeklyPromotion{}
	}
	return list, nil
}

func (s *Store) SaveWeeklyPromotion(wp models.WeeklyPromotion) (*models.WeeklyPromotion, error) {
	if wp.ProductID <= 0 {
		return nil, fmt.Errorf("selecione um produto do estoque para a promoção")
	}
	if wp.PromoPrice <= 0 {
		return nil, fmt.Errorf("informe o preço promocional (R$)")
	}
	if wp.DayOfWeek < 0 || wp.DayOfWeek > 7 {
		wp.DayOfWeek = 1
	}

	var pName, cName string
	var pPrice float64
	var pPromo sql.NullFloat64
	_ = s.DB.QueryRow(`SELECT name, category_name, price, promo_from FROM products WHERE id = ?`, wp.ProductID).Scan(&pName, &cName, &pPrice, &pPromo)
	if wp.OriginalPrice <= 0 {
		wp.OriginalPrice = pPrice
		if pPromo.Valid && pPromo.Float64 > wp.OriginalPrice {
			wp.OriginalPrice = pPromo.Float64
		}
	}
	// Se alguém digitou invertido (original menor que promo), corrige na hora!
	if wp.OriginalPrice > 0 && wp.OriginalPrice < wp.PromoPrice {
		wp.OriginalPrice, wp.PromoPrice = wp.PromoPrice, wp.OriginalPrice
	}

	actInt := 1
	if !wp.Active && wp.ID > 0 {
		actInt = 0
	} else {
		wp.Active = true
	}
	now := time.Now().Format("02/01 15:04")
	wp.UpdatedAt = now
	wp.ProductName = pName
	wp.CategoryName = cName
	wp.DayName = weekdayLabelPT(wp.DayOfWeek)

	if wp.ID > 0 {
		_, err := s.DB.Exec(`UPDATE weekly_promotions SET day_of_week = ?, product_id = ?, original_price = ?, promo_price = ?, active = ?, updated_at = ? WHERE id = ?`,
			wp.DayOfWeek, wp.ProductID, wp.OriginalPrice, wp.PromoPrice, actInt, now, wp.ID)
		return &wp, err
	}
	res, err := s.DB.Exec(`INSERT INTO weekly_promotions (day_of_week, product_id, original_price, promo_price, active, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		wp.DayOfWeek, wp.ProductID, wp.OriginalPrice, wp.PromoPrice, actInt, now)
	if err != nil {
		return nil, err
	}
	lastID, _ := res.LastInsertId()
	wp.ID = int(lastID)
	_ = s.AddSystemLog("INFO", "PROMO-SEMANA", fmt.Sprintf("Promoção programada para %s: %s (De R$ %.2f Por R$ %.2f)", wp.DayName, wp.ProductName, wp.OriginalPrice, wp.PromoPrice), "", "ADM")
	return &wp, nil
}

func (s *Store) DeleteWeeklyPromotion(id int) error {
	_, err := s.DB.Exec(`DELETE FROM weekly_promotions WHERE id = ?`, id)
	return err
}

func (s *Store) ToggleWeeklyPromotion(id int, active bool) error {
	actInt := 0
	if active {
		actInt = 1
	}
	_, err := s.DB.Exec(`UPDATE weekly_promotions SET active = ?, updated_at = ? WHERE id = ?`, actInt, time.Now().Format("02/01 15:04"), id)
	return err
}

func (s *Store) ListTables() ([]models.DiningTable, error) {
	rows, err := s.DB.Query(`SELECT id, number, label, COALESCE(complement,''), origin_prefix, COALESCE(is_kiosk,0), is_custom, status, COALESCE(waiter_call,0), qr_token, current_total, COALESCE(active, 1), updated_at
		FROM dining_tables ORDER BY number ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []models.DiningTable
	for rows.Next() {
		var t models.DiningTable
		var kiosk, custom, wCall, activeInt int
		if err := rows.Scan(&t.ID, &t.Number, &t.Label, &t.Complement, &t.OriginPrefix, &kiosk, &custom, &t.Status, &wCall, &t.QRToken, &t.CurrentTotal, &activeInt, &t.UpdatedAt); err == nil {
			t.IsKiosk = kiosk == 1
			t.IsCustom = custom == 1
			t.WaiterCall = wCall == 1
			t.Active = activeInt == 1
			if t.IsKiosk {
				t.QRToken = ""
			} else if strings.TrimSpace(t.QRToken) == "" {
				t.QRToken = fmt.Sprintf("SIRBEEF-MESA-%02d", t.Number)
			}
			if t.Complement != "" {
				t.Label = fmt.Sprintf("MESA %02d - \"%s\"", t.Number, t.Complement)
			} else {
				t.Label = fmt.Sprintf("MESA %02d", t.Number)
			}
			list = append(list, t)
		}
	}
	return list, nil
}

// UpdateTableComplement permite ao Garçom (ou ADM) editar apenas o complemento depois do número da mesa (ex: MESA 12 - "Na Calçada").
func (s *Store) UpdateTableComplement(tableID int, complement string) error {
	comp := strings.TrimSpace(complement)
	comp = strings.Trim(comp, `"'`)
	now := time.Now().Format("15:04")

	var num int
	if err := s.DB.QueryRow(`SELECT number FROM dining_tables WHERE id = ?`, tableID).Scan(&num); err != nil {
		return err
	}
	label := fmt.Sprintf("MESA %02d", num)
	if comp != "" {
		label = fmt.Sprintf("MESA %02d - \"%s\"", num, comp)
	}
	_, err := s.DB.Exec(`UPDATE dining_tables SET complement = ?, label = ?, updated_at = ? WHERE id = ?`, comp, label, now, tableID)
	return err
}

// AdminSaveTable cria ou edita uma mesa (número, complemento/identificação, tipo Normal QR Code vs Tablet Kiosk e status Ativa/Inativa) — Exclusivo ADMIN.
func (s *Store) AdminSaveTable(tableID int, number int, complement string, isKiosk bool, activeOpt ...bool) (*models.DiningTable, error) {
	comp := strings.TrimSpace(strings.Trim(complement, `"'`))
	now := time.Now().Format("15:04")
	kioskInt := 0
	if isKiosk {
		kioskInt = 1
	}

	if tableID > 0 {
		var curNum int
		var status string
		var waiterCall int
		var curTotal float64
		var curActive int
		err := s.DB.QueryRow(`SELECT number, status, waiter_call, current_total, COALESCE(active, 1) FROM dining_tables WHERE id = ?`, tableID).Scan(&curNum, &status, &waiterCall, &curTotal, &curActive)
		if err != nil {
			return nil, fmt.Errorf("mesa #%d não encontrada", tableID)
		}
		activeBool := curActive == 1
		if len(activeOpt) > 0 {
			activeBool = activeOpt[0]
		}
		activeInt := 0
		if activeBool {
			activeInt = 1
		}
		targetNum := number
		if targetNum <= 0 {
			targetNum = curNum
		}
		var dupID int
		if err := s.DB.QueryRow(`SELECT id FROM dining_tables WHERE number = ? AND id <> ?`, targetNum, tableID).Scan(&dupID); err == nil && dupID > 0 {
			return nil, fmt.Errorf("já existe outra mesa cadastrada com o número %02d", targetNum)
		}
		label := fmt.Sprintf("MESA %02d", targetNum)
		if comp != "" {
			label = fmt.Sprintf("MESA %02d - \"%s\"", targetNum, comp)
		}
		token := fmt.Sprintf("SIRBEEF-MESA-%02d", targetNum)
		if isKiosk {
			token = ""
		}
		_, err = s.DB.Exec(`UPDATE dining_tables SET number = ?, label = ?, complement = ?, is_kiosk = ?, qr_token = ?, active = ?, updated_at = ? WHERE id = ?`,
			targetNum, label, comp, kioskInt, token, activeInt, now, tableID)
		if err != nil {
			return nil, err
		}
		return &models.DiningTable{
			ID:           tableID,
			Number:       targetNum,
			Label:        label,
			Complement:   comp,
			IsKiosk:      isKiosk,
			Status:       status,
			WaiterCall:   waiterCall == 1,
			QRToken:      token,
			CurrentTotal: curTotal,
			Active:       activeBool,
			UpdatedAt:    now,
		}, nil
	}

	activeBool := true
	if len(activeOpt) > 0 {
		activeBool = activeOpt[0]
	}
	activeInt := 0
	if activeBool {
		activeInt = 1
	}

	targetNum := number
	if targetNum <= 0 {
		var maxNum int
		_ = s.DB.QueryRow(`SELECT COALESCE(MAX(number), 30) FROM dining_tables`).Scan(&maxNum)
		targetNum = maxNum + 1
	} else {
		var dupID int
		if err := s.DB.QueryRow(`SELECT id FROM dining_tables WHERE number = ?`, targetNum).Scan(&dupID); err == nil && dupID > 0 {
			return nil, fmt.Errorf("já existe a Mesa %02d cadastrada (edite a mesa existente ou escolha outro número)", targetNum)
		}
	}

	label := fmt.Sprintf("MESA %02d", targetNum)
	if comp != "" {
		label = fmt.Sprintf("MESA %02d - \"%s\"", targetNum, comp)
	}
	prefix := ""
	token := fmt.Sprintf("SIRBEEF-MESA-%02d", targetNum)
	if isKiosk {
		token = ""
	}
	res, err := s.DB.Exec(`INSERT INTO dining_tables (number, label, complement, origin_prefix, is_kiosk, is_custom, status, waiter_call, qr_token, current_total, active, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, 'LIVRE', 0, ?, 0, ?, ?)`, targetNum, label, comp, prefix, kioskInt, token, activeInt, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &models.DiningTable{
		ID:           int(id),
		Number:       targetNum,
		Label:        label,
		Complement:   comp,
		OriginPrefix: prefix,
		IsKiosk:      isKiosk,
		IsCustom:     false,
		Status:       "LIVRE",
		WaiterCall:   false,
		QRToken:      token,
		CurrentTotal: 0,
		Active:       activeBool,
		UpdatedAt:    now,
	}, nil
}

// AdminSetTableActive ativa ou inativa individualmente uma mesa (Exclusivo ADMIN).
func (s *Store) AdminSetTableActive(tableID int, active bool) error {
	actInt := 0
	if active {
		actInt = 1
	}
	now := time.Now().Format("15:04")
	_, err := s.DB.Exec(`UPDATE dining_tables SET active = ?, updated_at = ? WHERE id = ?`, actInt, now, tableID)
	if err == nil {
		stText := "INATIVADA (oculta p/ equipe)"
		if active {
			stText = "ATIVADA (visível p/ equipe)"
		}
		_ = s.AddSystemLog("INFO", "MESAS-ADM", fmt.Sprintf("Mesa ID #%d foi %s pelo ADM.", tableID, stText), "", "ADM")
	}
	return err
}

// AdminSetTablesActiveRange deixa visíveis apenas as mesas dentro da faixa [minNum .. maxNum] (ex: 15 até 30) — Exclusivo ADMIN.
func (s *Store) AdminSetTablesActiveRange(minNum int, maxNum int) error {
	if minNum <= 0 {
		minNum = 1
	}
	if maxNum < minNum {
		maxNum = minNum
	}
	now := time.Now().Format("15:04")
	_, err := s.DB.Exec(`UPDATE dining_tables SET active = CASE WHEN number >= ? AND number <= ? THEN 1 ELSE 0 END, updated_at = ?`, minNum, maxNum, now)
	if err == nil {
		_ = s.AddSystemLog("INFO", "MESAS-ADM", fmt.Sprintf("Faixa de mesas visíveis para a equipe atualizada pelo ADM: Mesa %02d até Mesa %02d.", minNum, maxNum), "", "ADM")
	}
	return err
}

// AdminCreateTable cria uma nova mesa (Exclusivo para o cargo ADMIN).
func (s *Store) AdminCreateTable(complement string, isKiosk bool) (*models.DiningTable, error) {
	return s.AdminSaveTable(0, 0, complement, isKiosk)
}

// AdminDeleteTable exclui uma mesa do sistema (Exclusivo para o cargo ADMIN).
func (s *Store) AdminDeleteTable(tableID int) error {
	_, err := s.DB.Exec(`DELETE FROM dining_tables WHERE id = ?`, tableID)
	return err
}

func (s *Store) CreateCustomTable(label string) (*models.DiningTable, error) {
	return s.AdminCreateTable(label, false)
}

func (s *Store) SetTableWaiterCall(tableID int, callActive bool, requestBill bool) error {
	now := time.Now().Format("15:04")
	wInt := 0
	if callActive {
		wInt = 1
	}
	realID := tableID
	var tNum int
	var tLabel string
	_ = s.DB.QueryRow(`SELECT id, number, label FROM dining_tables WHERE id = ? OR number = ? LIMIT 1`, tableID, tableID).Scan(&realID, &tNum, &tLabel)
	var err error
	if requestBill {
		_, err = s.DB.Exec(`UPDATE dining_tables SET waiter_call = ?, status = 'CONTA', updated_at = ? WHERE id = ?`, wInt, now, realID)
	} else {
		_, err = s.DB.Exec(`UPDATE dining_tables SET waiter_call = ?, updated_at = ? WHERE id = ?`, wInt, now, realID)
	}
	if callActive {
		cfg := s.GetPrinterConfig()
		if cfg.WaiterAlarmTableCall {
			if tLabel == "" {
				tNum = tableID
				tLabel = fmt.Sprintf("MESA %02d", tableID)
			}
			title := fmt.Sprintf("🙋 %s — CHAMANDO O GARÇOM!", tLabel)
			details := "O cliente solicitou atendimento presencial do garçom na mesa."
			if requestBill {
				title = fmt.Sprintf("💳 %s — SOLICITOU FECHAMENTO / CONTA!", tLabel)
				details = "O cliente pediu o fechamento da conta na mesa."
			}
			_, _ = s.CreateWaiterAlert(models.WaiterAlert{
				AlertType:   "CHAMADO_MESA",
				TableID:     realID,
				TableNumber: tNum,
				TableLabel:  tLabel,
				Title:       title,
				Details:     details,
			})
		}
	} else {
		_ = s.ConfirmWaiterAlert(0, tableID, "CHAMADO_MESA", "Garçom")
	}
	return err
}

func (s *Store) UpdateTableStatus(tableID int, status string, resetTotal bool) error {
	now := time.Now().Format("15:04")
	if resetTotal || status == "LIVRE" {
		_, _ = s.DB.Exec(`UPDATE comandas SET status = 'PAGA', closed_at = ? WHERE table_id = ? AND status != 'PAGA'`, time.Now().Format("02/01 15:04"), tableID)
		_, err := s.DB.Exec(`UPDATE dining_tables SET status = 'LIVRE', current_total = 0, waiter_call = 0, updated_at = ? WHERE id = ?`, now, tableID)
		_ = s.ConfirmWaiterAlert(0, tableID, "", "Sistema")
		return err
	}
	_, err := s.DB.Exec(`UPDATE dining_tables SET status = ?, updated_at = ? WHERE id = ?`, status, now, tableID)
	return err
}

func NormalizeComandaCode(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if strings.EqualFold(trimmed, "a1f5803f-0287-4944-83b4-a887422377b2") {
		return "CMD-001"
	}
	clean := strings.TrimSpace(strings.ToUpper(trimmed))
	clean = strings.TrimPrefix(clean, "SIRBEEF:COMANDA:")
	clean = strings.TrimPrefix(clean, "SIRBEEF-COMANDA-")
	clean = strings.TrimPrefix(clean, "COMANDA:")
	clean = strings.TrimPrefix(clean, "COMANDA-")
	clean = strings.TrimPrefix(clean, "COMANDA ")
	clean = strings.TrimPrefix(clean, "CMD-")
	clean = strings.TrimPrefix(clean, "CMD ")
	clean = strings.TrimPrefix(clean, "#")
	clean = strings.TrimSpace(clean)
	if num, err := strconv.Atoi(clean); err == nil && num > 0 {
		return fmt.Sprintf("CMD-%03d", num)
	}
	if clean == "" {
		return ""
	}
	if strings.HasPrefix(clean, "M") && strings.Contains(clean, "-") {
		return clean
	}
	return "CMD-" + clean
}

// ResolveComandaCode consulta primeiro a tabela de 100 cartões físicos (comanda_cards) pelo UUID do QR Code
// e se não encontrar, aplica NormalizeComandaCode.
func (s *Store) ResolveComandaCode(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	var mappedCode string
	err := s.DB.QueryRow(`SELECT comanda_code FROM comanda_cards WHERE LOWER(qr_payload) = LOWER(?) AND qr_payload != '' LIMIT 1`, trimmed).Scan(&mappedCode)
	if err == nil && mappedCode != "" {
		return mappedCode
	}
	return NormalizeComandaCode(trimmed)
}

func (s *Store) ListComandaCards() ([]models.ComandaCard, error) {
	rows, err := s.DB.Query(`SELECT card_number, comanda_code, COALESCE(qr_payload,''), COALESCE(updated_at,'') FROM comanda_cards ORDER BY card_number ASC`)
	if err != nil {
		return []models.ComandaCard{}, err
	}
	defer rows.Close()
	var list []models.ComandaCard
	for rows.Next() {
		var c models.ComandaCard
		if err := rows.Scan(&c.CardNumber, &c.ComandaCode, &c.QRPayload, &c.UpdatedAt); err == nil {
			list = append(list, c)
		}
	}
	if list == nil {
		list = []models.ComandaCard{}
	}
	return list, nil
}

func (s *Store) BindComandaCard(cardNumber int, qrPayload string) (*models.ComandaCard, error) {
	if cardNumber < 1 || cardNumber > 100 {
		return nil, fmt.Errorf("o número do cartão deve ser entre 001 e 100")
	}
	cleanQR := strings.TrimSpace(qrPayload)
	now := time.Now().Format("02/01/2006 15:04")
	cCode := fmt.Sprintf("CMD-%03d", cardNumber)
	// Remove esse mesmo QR de outro cartão caso tenha sido vinculado por engano antes
	if cleanQR != "" {
		_, _ = s.DB.Exec(`UPDATE comanda_cards SET qr_payload = '' WHERE LOWER(qr_payload) = LOWER(?) AND card_number != ?`, cleanQR, cardNumber)
	}
	_, err := s.DB.Exec(`INSERT INTO comanda_cards (card_number, comanda_code, qr_payload, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(card_number) DO UPDATE SET qr_payload = excluded.qr_payload, updated_at = excluded.updated_at`,
		cardNumber, cCode, cleanQR, now)
	if err != nil {
		return nil, err
	}
	return &models.ComandaCard{
		CardNumber:  cardNumber,
		ComandaCode: cCode,
		QRPayload:   cleanQR,
		UpdatedAt:   now,
	}, nil
}

func FirstTwoNames(fullName string) string {
	clean := strings.TrimSpace(fullName)
	if clean == "" {
		return ""
	}
	parts := strings.Fields(clean)
	if len(parts) <= 2 {
		return strings.Join(parts, " ")
	}
	return parts[0] + " " + parts[1]
}

func (s *Store) OpenOrGetComanda(tableID int, comandaCode, comandaType, customerName, customerPhone string) (*models.Comanda, error) {
	now := time.Now().Format("02/01 15:04")
	if tableID <= 0 {
		tableID = 1
	}
	var tableNum int
	var tableLbl string
	var isKiosk int
	err := s.DB.QueryRow(`SELECT number, label, COALESCE(is_kiosk,0) FROM dining_tables WHERE id = ? OR number = ? LIMIT 1`, tableID, tableID).Scan(&tableNum, &tableLbl, &isKiosk)
	if err != nil {
		tableNum = tableID
		tableLbl = fmt.Sprintf("MESA %02d", tableNum)
	}

	cName := strings.TrimSpace(customerName)
	cPhone := strings.TrimSpace(customerPhone)
	normCode := s.ResolveComandaCode(comandaCode)
	if strings.HasPrefix(strings.ToUpper(normCode), "M") && strings.Contains(normCode, "-") {
		normCode = strings.Split(normCode, "-")[0]
	}

	if comandaType == "" {
		if isKiosk == 1 || (normCode != "" && strings.HasPrefix(normCode, "CMD-")) {
			comandaType = "QR_TABLET"
		} else {
			comandaType = "NOME_CELULAR"
		}
	}

	if comandaType == "NOME_CELULAR" && (normCode == "" || strings.HasPrefix(strings.ToUpper(normCode), "M")) {
		normCode = fmt.Sprintf("M%02d", tableNum)
	}
	if normCode == "" {
		normCode = fmt.Sprintf("CMD-%03d", tableNum)
	}
	if cName == "" {
		if strings.HasPrefix(normCode, "CMD-") {
			cName = "Cartão " + normCode
		} else {
			cName = "Cliente " + tableLbl
		}
	}

	// Verifica se já existe comanda aberta (ABERTA ou FECHAMENTO_SOLICITADO)
	var existing models.Comanda
	if comandaType == "NOME_CELULAR" {
		err = s.DB.QueryRow(`SELECT id, comanda_code, comanda_type, table_id, table_number, table_label, customer_name, customer_phone, status, payment_method, total, orders_count, opened_at, closed_at
			FROM comandas
			WHERE table_id = ? AND status != 'PAGA'
			  AND ((? != '' AND customer_phone = ?) OR UPPER(customer_name) = UPPER(?))
			ORDER BY id DESC LIMIT 1`, tableID, cPhone, cPhone, cName).Scan(
			&existing.ID, &existing.ComandaCode, &existing.ComandaType, &existing.TableID, &existing.TableNumber, &existing.TableLabel,
			&existing.CustomerName, &existing.CustomerPhone, &existing.Status, &existing.PaymentMethod, &existing.Total, &existing.OrdersCount,
			&existing.OpenedAt, &existing.ClosedAt,
		)
	} else {
		err = s.DB.QueryRow(`SELECT id, comanda_code, comanda_type, table_id, table_number, table_label, customer_name, customer_phone, status, payment_method, total, orders_count, opened_at, closed_at
			FROM comandas WHERE UPPER(comanda_code) = UPPER(?) AND status != 'PAGA' ORDER BY id DESC LIMIT 1`, normCode).Scan(
			&existing.ID, &existing.ComandaCode, &existing.ComandaType, &existing.TableID, &existing.TableNumber, &existing.TableLabel,
			&existing.CustomerName, &existing.CustomerPhone, &existing.Status, &existing.PaymentMethod, &existing.Total, &existing.OrdersCount,
			&existing.OpenedAt, &existing.ClosedAt,
		)
	}
	if err == nil && existing.ID > 0 {
		// Atualiza mesa ou nome/celular caso tenham sido preenchidos agora
		if cName != "" && !strings.HasPrefix(cName, "Cartão ") && !strings.HasPrefix(cName, "Cliente MESA") {
			existing.CustomerName = cName
		}
		if cPhone != "" {
			existing.CustomerPhone = cPhone
		}
		existing.ComandaCode = normCode
		existing.TableID = tableID
		existing.TableNumber = tableNum
		existing.TableLabel = tableLbl
		_, _ = s.DB.Exec(`UPDATE comandas SET comanda_code = ?, table_id = ?, table_number = ?, table_label = ?, customer_name = ?, customer_phone = ? WHERE id = ?`,
			existing.ComandaCode, existing.TableID, existing.TableNumber, existing.TableLabel, existing.CustomerName, existing.CustomerPhone, existing.ID)
		_, _ = s.DB.Exec(`UPDATE dining_tables SET status = CASE WHEN status = 'LIVRE' THEN 'OCUPADA' ELSE status END, updated_at = ? WHERE id = ?`,
			time.Now().Format("15:04"), tableID)
		return &existing, nil
	}

	res, err := s.DB.Exec(`INSERT INTO comandas (
		comanda_code, comanda_type, table_id, table_number, table_label, customer_name, customer_phone, status, payment_method, total, orders_count, opened_at, closed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, 'ABERTA', 'PENDENTE', 0, 0, ?, '')`,
		normCode, comandaType, tableID, tableNum, tableLbl, cName, cPhone, now,
	)
	if err != nil {
		return nil, err
	}
	cid, _ := res.LastInsertId()
	_, _ = s.DB.Exec(`UPDATE dining_tables SET status = CASE WHEN status = 'LIVRE' THEN 'OCUPADA' ELSE status END, updated_at = ? WHERE id = ?`,
		time.Now().Format("15:04"), tableID)

	return &models.Comanda{
		ID:            int(cid),
		ComandaCode:   normCode,
		ComandaType:   comandaType,
		TableID:       tableID,
		TableNumber:   tableNum,
		TableLabel:    tableLbl,
		CustomerName:  cName,
		CustomerPhone: cPhone,
		Status:        "ABERTA",
		PaymentMethod: "PENDENTE",
		Total:         0,
		OrdersCount:   0,
		Items:         []models.OrderItem{},
		OpenedAt:      now,
	}, nil
}

func (s *Store) recalculateComandaAndTable(comandaID int, tableID int) {
	if comandaID > 0 {
		var sumTotal float64
		var cnt int
		_ = s.DB.QueryRow(`SELECT COALESCE(SUM(total), 0), COUNT(*) FROM orders WHERE comanda_id = ? AND status != 'CANCELADO'`, comandaID).Scan(&sumTotal, &cnt)
		_, _ = s.DB.Exec(`UPDATE comandas SET total = ?, orders_count = ? WHERE id = ?`, math.Round(sumTotal*100)/100, cnt, comandaID)
	}
	if tableID > 0 {
		var openTotal float64
		var openCount int
		var closingCount int
		_ = s.DB.QueryRow(`SELECT COALESCE(SUM(total), 0), COUNT(*) FROM comandas WHERE table_id = ? AND status != 'PAGA'`, tableID).Scan(&openTotal, &openCount)
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM comandas WHERE table_id = ? AND status = 'FECHAMENTO_SOLICITADO'`, tableID).Scan(&closingCount)
		now := time.Now().Format("15:04")
		if openCount == 0 && openTotal <= 0.01 {
			_, _ = s.DB.Exec(`UPDATE dining_tables SET status = 'LIVRE', current_total = 0, waiter_call = 0, updated_at = ? WHERE id = ?`, now, tableID)
		} else {
			newSt := "OCUPADA"
			if closingCount > 0 {
				newSt = "CONTA"
			}
			_, _ = s.DB.Exec(`UPDATE dining_tables SET status = ?, current_total = ?, updated_at = ? WHERE id = ?`,
				newSt, math.Round(openTotal*100)/100, now, tableID)
		}
	}
}

// closeEmptyComandaAndRecalcTable encerra/remove automaticamente uma comanda aberta quando todos os seus pedidos/itens
// foram excluídos ou cancelados (total R$ 0,00 e 0 pedidos ativos), liberando a mesa de volta para LIVRE.
func (s *Store) closeEmptyComandaAndRecalcTable(comandaID int, tableID int) {
	if comandaID > 0 && tableID <= 0 {
		_ = s.DB.QueryRow(`SELECT COALESCE(table_id, 0) FROM comandas WHERE id = ?`, comandaID).Scan(&tableID)
	}
	if comandaID > 0 {
		var activeOrders int
		var sumTotal float64
		_ = s.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(total), 0) FROM orders WHERE comanda_id = ? AND status != 'CANCELADO'`, comandaID).Scan(&activeOrders, &sumTotal)
		if activeOrders == 0 && sumTotal <= 0.009 {
			_, _ = s.DB.Exec(`DELETE FROM comandas WHERE id = ? AND status != 'PAGA'`, comandaID)
			comandaID = 0
		}
	}
	if tableID > 0 {
		_, _ = s.DB.Exec(`DELETE FROM comandas WHERE table_id = ? AND status != 'PAGA' AND NOT EXISTS (
			SELECT 1 FROM orders o WHERE o.comanda_id = comandas.id AND o.status != 'CANCELADO'
		)`, tableID)
	}
	s.recalculateComandaAndTable(comandaID, tableID)
}

// CleanupZeroOrphanComandas limpa comandas abertas órfãs sem nenhum pedido ativo (R$ 0,00) e recalcula o status das mesas.
func (s *Store) CleanupZeroOrphanComandas() {
	_, _ = s.DB.Exec(`DELETE FROM comandas WHERE status != 'PAGA' AND NOT EXISTS (
		SELECT 1 FROM orders o WHERE o.comanda_id = comandas.id AND o.status != 'CANCELADO'
	)`)
	rows, err := s.DB.Query(`SELECT id, COALESCE(table_id, 0) FROM comandas WHERE status != 'PAGA'`)
	if err == nil {
		type pair struct{ cid, tid int }
		var list []pair
		for rows.Next() {
			var p pair
			if rows.Scan(&p.cid, &p.tid) == nil {
				list = append(list, p)
			}
		}
		rows.Close()
		for _, p := range list {
			s.recalculateComandaAndTable(p.cid, p.tid)
		}
	}
	tRows, err := s.DB.Query(`SELECT id FROM dining_tables`)
	if err == nil {
		var tids []int
		for tRows.Next() {
			var tid int
			if tRows.Scan(&tid) == nil {
				tids = append(tids, tid)
			}
		}
		tRows.Close()
		for _, tid := range tids {
			s.recalculateComandaAndTable(0, tid)
		}
	}
}

func (s *Store) ListComandas(includePaid bool) ([]models.Comanda, error) {
	query := `SELECT id, comanda_code, comanda_type, table_id, table_number, table_label, customer_name, customer_phone, status, payment_method, total, orders_count, opened_at, closed_at
		FROM comandas`
	if !includePaid {
		query += ` WHERE status != 'PAGA'`
	}
	query += ` ORDER BY CASE WHEN status = 'FECHAMENTO_SOLICITADO' THEN 0 WHEN status = 'ABERTA' THEN 1 ELSE 2 END, table_number ASC, id DESC`

	rows, err := s.DB.Query(query)
	if err != nil {
		return []models.Comanda{}, err
	}
	defer rows.Close()

	var list []models.Comanda
	for rows.Next() {
		var c models.Comanda
		if err := rows.Scan(
			&c.ID, &c.ComandaCode, &c.ComandaType, &c.TableID, &c.TableNumber, &c.TableLabel,
			&c.CustomerName, &c.CustomerPhone, &c.Status, &c.PaymentMethod, &c.Total, &c.OrdersCount,
			&c.OpenedAt, &c.ClosedAt,
		); err == nil {
			if strings.HasPrefix(strings.ToUpper(c.ComandaCode), "M") && strings.Contains(c.ComandaCode, "-") {
				c.ComandaCode = strings.Split(c.ComandaCode, "-")[0]
			}
			c.Items = s.getComandaItems(c.ID)
			list = append(list, c)
		}
	}
	if list == nil {
		list = []models.Comanda{}
	}
	return list, nil
}

func (s *Store) getComandaItems(comandaID int) []models.OrderItem {
	rows, err := s.DB.Query(`SELECT oi.id, oi.order_id, oi.product_id, oi.product_name, oi.quantity, oi.unit_price, oi.total_price, oi.selected_modifiers_json, oi.notes, oi.sector_abc
		FROM order_items oi
		INNER JOIN orders o ON o.id = oi.order_id
		WHERE o.comanda_id = ? AND o.status != 'CANCELADO'
		ORDER BY oi.id ASC`, comandaID)
	if err != nil {
		return []models.OrderItem{}
	}
	defer rows.Close()

	var items []models.OrderItem
	for rows.Next() {
		var it models.OrderItem
		var modJSON string
		if err := rows.Scan(&it.ID, &it.OrderID, &it.ProductID, &it.ProductName, &it.Quantity, &it.UnitPrice, &it.TotalPrice, &modJSON, &it.Notes, &it.SectorABC); err == nil {
			_ = json.Unmarshal([]byte(modJSON), &it.SelectedModifiers)
			if it.SelectedModifiers == nil {
				it.SelectedModifiers = []models.SelectedModifier{}
			}
			items = append(items, it)
		}
	}
	if items == nil {
		items = []models.OrderItem{}
	}
	return items
}

func (s *Store) RequestCloseComanda(comandaID int, comandaCode string) (*models.Comanda, error) {
	if comandaID <= 0 && comandaCode != "" {
		norm := s.ResolveComandaCode(comandaCode)
		_ = s.DB.QueryRow(`SELECT id FROM comandas WHERE UPPER(comanda_code) = UPPER(?) AND status != 'PAGA' ORDER BY id DESC LIMIT 1`, norm).Scan(&comandaID)
	}
	if comandaID <= 0 {
		return nil, fmt.Errorf("comanda ativa não encontrada")
	}
	var tableID int
	_ = s.DB.QueryRow(`SELECT table_id FROM comandas WHERE id = ?`, comandaID).Scan(&tableID)
	_, err := s.DB.Exec(`UPDATE comandas SET status = 'FECHAMENTO_SOLICITADO' WHERE id = ?`, comandaID)
	if err != nil {
		return nil, err
	}
	if tableID > 0 {
		_, _ = s.DB.Exec(`UPDATE dining_tables SET status = 'CONTA', waiter_call = 1, updated_at = ? WHERE id = ?`, time.Now().Format("15:04"), tableID)
		s.recalculateComandaAndTable(comandaID, tableID)
	}
	list, _ := s.ListComandas(true)
	for _, c := range list {
		if c.ID == comandaID {
			return &c, nil
		}
	}
	return nil, nil
}

func (s *Store) RemoveComandaItem(comandaID int, orderItemID int) (*models.Comanda, error) {
	var orderID int
	if err := s.DB.QueryRow(`SELECT order_id FROM order_items WHERE id = ?`, orderItemID).Scan(&orderID); err != nil {
		return nil, err
	}
	_, _ = s.DB.Exec(`DELETE FROM order_items WHERE id = ?`, orderItemID)
	var remSum float64
	var remCnt int
	_ = s.DB.QueryRow(`SELECT COALESCE(SUM(total_price), 0), COUNT(*) FROM order_items WHERE order_id = ?`, orderID).Scan(&remSum, &remCnt)
	if remCnt == 0 {
		_, _ = s.DB.Exec(`UPDATE orders SET status = 'CANCELADO', subtotal = 0, total = 0 WHERE id = ?`, orderID)
	} else {
		_, _ = s.DB.Exec(`UPDATE orders SET subtotal = ?, total = ? + COALESCE(delivery_fee, 0) WHERE id = ?`, remSum, remSum, orderID)
	}

	var tableID int
	_ = s.DB.QueryRow(`SELECT table_id FROM comandas WHERE id = ?`, comandaID).Scan(&tableID)
	s.closeEmptyComandaAndRecalcTable(comandaID, tableID)

	list, _ := s.ListComandas(true)
	for _, c := range list {
		if c.ID == comandaID {
			return &c, nil
		}
	}
	return nil, nil
}

func (s *Store) PayAndCloseComanda(comandaID int, paymentMethod string, cashierIDOpt ...int) (*models.Comanda, error) {
	if paymentMethod == "" {
		paymentMethod = "PIX"
	}
	now := time.Now().Format("02/01 15:04")
	nowISO := time.Now().Format("2006-01-02 15:04:05")
	var tableID int
	var cmdTotal float64
	var cmdCode, custName string
	if err := s.DB.QueryRow(`SELECT COALESCE(table_id, 0), COALESCE(total, 0), COALESCE(comanda_code, ''), COALESCE(customer_name, '') FROM comandas WHERE id = ?`, comandaID).Scan(&tableID, &cmdTotal, &cmdCode, &custName); err != nil {
		return nil, err
	}
	var activeOrders int
	var activeSum float64
	_ = s.DB.QueryRow(`SELECT COUNT(*), COALESCE(SUM(total), 0) FROM orders WHERE comanda_id = ? AND status != 'CANCELADO'`, comandaID).Scan(&activeOrders, &activeSum)
	if (cmdTotal <= 0.009 && activeSum <= 0.009 && activeOrders == 0) || strings.EqualFold(paymentMethod, "CANCELADA_ZERADA") {
		_, _ = s.DB.Exec(`DELETE FROM comandas WHERE id = ?`, comandaID)
		s.recalculateComandaAndTable(0, tableID)
		return &models.Comanda{
			ID:            comandaID,
			ComandaCode:   cmdCode,
			TableID:       tableID,
			CustomerName:  custName,
			Status:        "PAGA",
			PaymentMethod: "CANCELADA_ZERADA",
			Total:         0,
			OrdersCount:   0,
			Items:         []models.OrderItem{},
			ClosedAt:      now,
		}, nil
	}
	_, err := s.DB.Exec(`UPDATE comandas SET status = 'PAGA', payment_method = ?, closed_at = ? WHERE id = ?`, paymentMethod, now, comandaID)
	if err != nil {
		return nil, err
	}
	cid := 0
	if len(cashierIDOpt) > 0 {
		cid = cashierIDOpt[0]
	}
	if cid <= 0 {
		_ = s.DB.QueryRow(`SELECT cashier_id FROM cashier_shifts WHERE status = 'ABERTO' ORDER BY id ASC LIMIT 1`).Scan(&cid)
		if cid <= 0 {
			cid = 2
		}
	}
	var activeShiftID int
	_ = s.DB.QueryRow(`SELECT id FROM cashier_shifts WHERE cashier_id = ? LIMIT 1`, cid).Scan(&activeShiftID)
	if activeShiftID <= 0 {
		activeShiftID = 1
	}
	_, _ = s.DB.Exec(`UPDATE orders SET status = 'ENTREGUE', status_pagamento = 'PAGO', payment_method = ?, cashier_id = ?, shift_id = ?, created_at_iso = ?, updated_at = ? WHERE comanda_id = ? AND status != 'CANCELADO'`,
		paymentMethod, cid, activeShiftID, nowISO, now, comandaID)
	s.recalculateComandaAndTable(comandaID, tableID)
	s.RecalcCashierShiftTotals()

	list, _ := s.ListComandas(true)
	for _, c := range list {
		if c.ID == comandaID {
			return &c, nil
		}
	}
	return nil, nil
}

func (s *Store) CreateOrder(o models.Order) (*models.Order, error) {
	now := time.Now().Format("02/01/2006 15:04")
	nowISO := time.Now().Format("2006-01-02 15:04:05")
	if o.OriginType == "" {
		o.OriginType = "DELIVERY"
	}
	if o.WaiterID != nil && *o.WaiterID > 0 && strings.TrimSpace(o.WaiterName) == "" {
		_ = s.DB.QueryRow(`SELECT name FROM users WHERE id = ? LIMIT 1`, *o.WaiterID).Scan(&o.WaiterName)
	}
	if strings.EqualFold(o.OriginType, "GARCOM") && strings.TrimSpace(o.WaiterName) == "" {
		wid := 3
		o.WaiterID = &wid
		o.WaiterName = "Lucas (Garçom Salão & Deck)"
	}
	if strings.Contains(strings.ToUpper(o.HeaderTag), "RETIRADA") || strings.Contains(strings.ToUpper(o.AddressText), "RETIRADA") {
		if strings.EqualFold(o.OriginType, "DELIVERY") {
			o.OriginType = "RETIRADA"
		}
	}
	if o.HeaderTag == "" {
		switch strings.ToUpper(o.OriginType) {
		case "DELIVERY":
			o.HeaderTag = "[DELIVERY]"
		case "RETIRADA":
			o.HeaderTag = "[RETIRADA BALCÃO]"
		case "PDV_MERCADO":
			o.HeaderTag = "[PDV MERCADO]"
		default:
			o.HeaderTag = "MESA"
		}
	}
	if o.Status == "" || strings.EqualFold(o.Status, "RECEBIDO") {
		o.Status = "PREPARANDO"
	}
	if o.PaymentMethod == "" {
		o.PaymentMethod = "DINHEIRO"
	}
	if o.StatusPagamento == "" {
		o.StatusPagamento = "PENDENTE"
	}
	if o.Latitude == 0 {
		o.Latitude = -23.55812
	}
	if o.Longitude == 0 {
		o.Longitude = -46.66012
	}

	var subtotal float64
	hasKitchenItem := false
	for i := range o.Items {
		if o.Items[i].Quantity <= 0 {
			o.Items[i].Quantity = 1
		}
		var modSum float64
		for _, m := range o.Items[i].SelectedModifiers {
			modSum += m.Price
		}
		if o.Items[i].TotalPrice <= 0 {
			o.Items[i].TotalPrice = math.Round((o.Items[i].UnitPrice+modSum)*o.Items[i].Quantity*100) / 100
		}
		var pSector, pSectorABC string
		var pCatID int
		if o.Items[i].ProductID > 0 {
			_ = s.DB.QueryRow(`SELECT COALESCE(sector,''), COALESCE(sector_abc,''), COALESCE(category_id,0) FROM products WHERE id = ?`, o.Items[i].ProductID).Scan(&pSector, &pSectorABC, &pCatID)
		}
		if o.Items[i].SectorABC == "" {
			if strings.EqualFold(pSector, "MERCADO") || strings.EqualFold(pSectorABC, "MERCADO_CARNES") || pCatID == 900 {
				o.Items[i].SectorABC = "MERCADO_CARNES"
			} else {
				o.Items[i].SectorABC = "HAMBURGUERIA"
			}
		}
		if !strings.EqualFold(o.Items[i].SectorABC, "MERCADO_CARNES") && !strings.EqualFold(pSector, "MERCADO") && pCatID != 900 {
			hasKitchenItem = true
		}
		subtotal += o.Items[i].TotalPrice
	}
	if hasKitchenItem {
		// Qualquer venda de Balcão (PDV_MERCADO / BALCAO) ou Retirada que contenha lanches/itens da cozinha
		// deve entrar obrigatoriamente como PREPARANDO na Cozinha / Expedição!
		if strings.EqualFold(o.OriginType, "PDV_MERCADO") || strings.EqualFold(o.OriginType, "BALCAO") || strings.EqualFold(o.OriginType, "RETIRADA") {
			o.Status = "PREPARANDO"
			if strings.EqualFold(o.OriginType, "PDV_MERCADO") && (o.HeaderTag == "" || o.HeaderTag == "[PDV MERCADO]") {
				o.HeaderTag = "[BALCÃO COZINHA]"
			}
		}
	}
	o.Subtotal = subtotal
	o.CouponCode = strings.ToUpper(strings.TrimSpace(o.CouponCode))
	if o.CouponCode != "" && o.DiscountAmount <= 0 {
		if _, calcDisc, errCup := s.ValidateDiscountCoupon(o.CouponCode, o.Subtotal, o.CustomerPhone); errCup == nil && calcDisc > 0 {
			o.DiscountAmount = calcDisc
		}
	}
	if o.DiscountAmount < 0 {
		o.DiscountAmount = 0
	}
	if o.DiscountAmount > (subtotal + o.DeliveryFee) {
		o.DiscountAmount = subtotal + o.DeliveryFee
	}
	o.DiscountAmount = math.Round(o.DiscountAmount*100) / 100
	o.Total = math.Max(0, math.Round((subtotal+o.DeliveryFee-o.DiscountAmount)*100)/100)
	o.CreatedAt = now
	o.CreatedAtISO = nowISO
	o.UpdatedAt = now

	if o.OrderCode == "" {
		o.OrderCode = fmt.Sprintf("SB-%d", 1045+(time.Now().Unix()%8900))
	}

	if o.CashierID == nil || *o.CashierID <= 0 {
		var openCid int
		_ = s.DB.QueryRow(`SELECT cashier_id FROM cashier_shifts WHERE status = 'ABERTO' ORDER BY id ASC LIMIT 1`).Scan(&openCid)
		if openCid <= 0 {
			openCid = 2
		}
		o.CashierID = &openCid
	}
	if o.ShiftID <= 0 {
		_ = s.DB.QueryRow(`SELECT id FROM cashier_shifts WHERE cashier_id = ? LIMIT 1`, *o.CashierID).Scan(&o.ShiftID)
		if o.ShiftID <= 0 {
			_ = s.DB.QueryRow(`SELECT id FROM cashier_shifts WHERE status = 'ABERTO' ORDER BY id ASC LIMIT 1`).Scan(&o.ShiftID)
		}
		if o.ShiftID <= 0 {
			o.ShiftID = 1
		}
	}

	// Se for pedido de Mesa / Tablet / Garçom ou possuir ComandaID / ComandaCode, vincula automaticamente à Comanda Individual do cliente!
	isTableOrder := (o.TableID != nil && *o.TableID > 0) ||
		(o.ComandaID != nil && *o.ComandaID > 0) ||
		strings.EqualFold(o.OriginType, "TABLET") ||
		strings.EqualFold(o.OriginType, "MESA") ||
		strings.EqualFold(o.OriginType, "GARCOM") ||
		strings.TrimSpace(o.ComandaCode) != ""

	if isTableOrder {
		var cmd *models.Comanda
		if o.ComandaID != nil && *o.ComandaID > 0 {
			var existing models.Comanda
			errScan := s.DB.QueryRow(`SELECT id, comanda_code, comanda_type, table_id, table_number, table_label, customer_name, customer_phone, status, payment_method, total, orders_count, opened_at, closed_at
				FROM comandas WHERE id = ? LIMIT 1`, *o.ComandaID).Scan(
				&existing.ID, &existing.ComandaCode, &existing.ComandaType, &existing.TableID, &existing.TableNumber, &existing.TableLabel,
				&existing.CustomerName, &existing.CustomerPhone, &existing.Status, &existing.PaymentMethod, &existing.Total, &existing.OrdersCount,
				&existing.OpenedAt, &existing.ClosedAt,
			)
			if errScan == nil && existing.ID > 0 {
				cmd = &existing
			}
		}
		if cmd == nil {
			tid := 1
			if o.TableID != nil && *o.TableID > 0 {
				tid = *o.TableID
			}
			cType := "NOME_CELULAR"
			if strings.EqualFold(o.OriginType, "TABLET") || (strings.TrimSpace(o.ComandaCode) != "" && !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(o.ComandaCode)), "M")) {
				cType = "QR_TABLET"
			}
			cmd, _ = s.OpenOrGetComanda(tid, o.ComandaCode, cType, o.CustomerName, o.CustomerPhone)
		}
		if cmd != nil {
			cid := cmd.ID
			o.ComandaID = &cid
			o.ComandaCode = cmd.ComandaCode
			o.TableID = &cmd.TableID
			if o.CustomerName == "" || strings.HasPrefix(o.CustomerName, "Cliente MESA") {
				o.CustomerName = cmd.CustomerName
			}
			if o.CustomerPhone == "" {
				o.CustomerPhone = cmd.CustomerPhone
			}
			if cmd.ComandaType == "QR_TABLET" {
				o.HeaderTag = fmt.Sprintf("%s • %s", cmd.TableLabel, cmd.ComandaCode)
			} else {
				shortName := FirstTwoNames(cmd.CustomerName)
				if shortName != "" {
					o.HeaderTag = fmt.Sprintf("%s %s", cmd.ComandaCode, shortName)
				} else {
					o.HeaderTag = cmd.ComandaCode
				}
			}
		}
	}

	res, err := s.DB.Exec(`INSERT INTO orders (
		order_code, origin_type, header_tag, table_id, comanda_id, comanda_code, customer_name, customer_phone,
		address_text, latitude, longitude, status, payment_method, gateway_id, status_pagamento,
		change_for, subtotal, delivery_fee, coupon_code, discount_amount, total, motoboy_id, motoboy_name, waiter_id, waiter_name, cashier_id, shift_id, qr_payload, created_at, created_at_iso, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
		o.OrderCode, o.OriginType, o.HeaderTag, o.TableID, o.ComandaID, o.ComandaCode, o.CustomerName, o.CustomerPhone,
		o.AddressText, o.Latitude, o.Longitude, o.Status, o.PaymentMethod, o.GatewayID, o.StatusPagamento,
		o.ChangeFor, o.Subtotal, o.DeliveryFee, o.CouponCode, o.DiscountAmount, o.Total, o.MotoboyID, o.MotoboyName, o.WaiterID, o.WaiterName, o.CashierID, o.ShiftID, o.CreatedAt, o.CreatedAtISO, o.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	id64, _ := res.LastInsertId()
	o.ID = int(id64)
	if o.CouponCode != "" && o.DiscountAmount > 0 {
		s.MarkDiscountCouponUsed(o.CouponCode, o.ID)
	}
	if strings.EqualFold(o.OriginType, "DELIVERY") {
		o.QRPayload = fmt.Sprintf("SIRBEEF:ORDER:%d", o.ID)
	} else {
		o.QRPayload = ""
	}
	_, _ = s.DB.Exec(`UPDATE orders SET qr_payload = ? WHERE id = ?`, o.QRPayload, o.ID)

	for _, it := range o.Items {
		modJSON, _ := json.Marshal(it.SelectedModifiers)
		_, _ = s.DB.Exec(`INSERT INTO order_items (
			order_id, product_id, product_name, quantity, unit_price, total_price, selected_modifiers_json, notes, sector_abc
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			o.ID, it.ProductID, it.ProductName, it.Quantity, it.UnitPrice, it.TotalPrice, string(modJSON), it.Notes, it.SectorABC,
		)
		// Baixa automática de estoque quando o produto possui controle de estoque ativo (exceto se stock_qty < 0, que representa Estoque Infinito -1)
		if it.ProductID > 0 && it.Quantity > 0 {
			_, _ = s.DB.Exec(`UPDATE products SET
				stock_qty = CASE WHEN COALESCE(stock_qty, 40.0) < 0 THEN -1 ELSE MAX(0, COALESCE(stock_qty, 40.0) - ?) END,
				active = CASE WHEN COALESCE(stock_qty, 40.0) < 0 THEN active WHEN COALESCE(track_stock, 1) = 1 AND (COALESCE(stock_qty, 40.0) - ?) <= 0 THEN 0 ELSE active END
				WHERE id = ?`, it.Quantity, it.Quantity, it.ProductID)
		}
	}

	// Recalcula o saldo da Comanda Individual e o saldo total da Mesa
	if o.ComandaID != nil && *o.ComandaID > 0 {
		tid := 0
		if o.TableID != nil {
			tid = *o.TableID
		}
		s.recalculateComandaAndTable(*o.ComandaID, tid)
	} else if o.TableID != nil && *o.TableID > 0 {
		_, _ = s.DB.Exec(`UPDATE dining_tables SET status = 'OCUPADA', current_total = current_total + ?, updated_at = ? WHERE id = ?`,
			o.Total, time.Now().Format("15:04"), *o.TableID)
	}

	// Gera preview do cupom ESC/POS e tickets separados por Impressora / Categoria (Bar vs Cozinha/Chapa vs Mercado)
	cfg := s.GetPrinterConfig()
	o.ReceiptPreview = escpos.RenderTextReceipt(o, cfg.Columns)
	o.SectorTickets = s.BuildOrderSectorTickets(o)

	// Se o Alarme de Bebidas do Garçom estiver ATIVO e o pedido tiver bebidas (Salão/Mesa/Comanda/Tablet/Balcão),
	// dispara imediatamente o alerta de BEBIDAS_MESA para o APK do Garçom mostrando a Mesa e as Bebidas!
	if cfg.WaiterAlarmDrinks {
		drinkItems := s.extractDrinkItems(o.Items)
		if len(drinkItems) > 0 {
			tID := 0
			tNum := 0
			tLbl := o.HeaderTag
			if o.TableID != nil && *o.TableID > 0 {
				tID = *o.TableID
				_ = s.DB.QueryRow(`SELECT number, label FROM dining_tables WHERE id = ? OR number = ? LIMIT 1`, tID, tID).Scan(&tNum, &tLbl)
			}
			if tLbl == "" {
				if tID > 0 {
					tLbl = fmt.Sprintf("MESA %02d", tID)
				} else {
					tLbl = o.HeaderTag
				}
			}
			var drinkLines []string
			for _, dit := range drinkItems {
				qStr := fmt.Sprintf("%.0fx", dit.Quantity)
				if math.Mod(dit.Quantity, 1.0) > 0.01 {
					qStr = fmt.Sprintf("%.2fx", dit.Quantity)
				}
				line := fmt.Sprintf("%s %s", qStr, dit.ProductName)
				var mods []string
				for _, m := range dit.SelectedModifiers {
					mods = append(mods, m.OptionName)
				}
				if len(mods) > 0 {
					line += " (" + strings.Join(mods, ", ") + ")"
				}
				if strings.TrimSpace(dit.Notes) != "" {
					line += " [Obs: " + strings.TrimSpace(dit.Notes) + "]"
				}
				drinkLines = append(drinkLines, line)
			}
			_, _ = s.CreateWaiterAlert(models.WaiterAlert{
				AlertType:    "BEBIDAS_MESA",
				TableID:      tID,
				TableNumber:  tNum,
				TableLabel:   tLbl,
				OrderID:      o.ID,
				OrderCode:    o.OrderCode,
				ComandaCode:  o.ComandaCode,
				CustomerName: o.CustomerName,
				Title:        fmt.Sprintf("🍺 %s — LEVAR BEBIDAS (%s)", tLbl, o.OrderCode),
				Details:      strings.Join(drinkLines, " • "),
				Items:        drinkItems,
			})
		}
	}

	// Se o pedido já entrou PAGO no balcão/caixa, recalcula o turno do operador de caixa
	s.RecalcCashierShiftTotals()

	// Atualiza automaticamente o cadastro de Cliente CRM (Delivery e Retirada + Endereço IA + Brindes)
	s.SyncCustomerFromOrder(o)

	_ = s.AddSystemLog("INFO", "PEDIDO-CRIADO", fmt.Sprintf("Novo pedido #%s (%s) • Cliente: %s • Total: R$ %.2f", o.OrderCode, o.OriginType, o.CustomerName, o.Total), "", "HOST-DB")

	// Dispara comprovante automático no WhatsApp EXCLUSIVAMENTE para pedidos feitos em DELIVERY (nunca para Mesa/Comanda/Salão)
	if strings.EqualFold(o.OriginType, "DELIVERY") && strings.TrimSpace(o.CustomerPhone) != "" {
		msg := whatsapp.BuildOrderStatusMessage(o)
		s.SendAndLogWhatsApp(o.CustomerPhone, o.CustomerName, "COMPROVANTE_"+o.Status, msg)
	}

	return &o, nil
}

func (s *Store) ListOrders(statusFilter string) ([]models.Order, error) {
	query := `SELECT id, order_code, origin_type, header_tag, table_id, comanda_id, COALESCE(comanda_code,''), customer_name, customer_phone,
		address_text, latitude, longitude, status, payment_method, gateway_id, status_pagamento,
		change_for, subtotal, delivery_fee, COALESCE(coupon_code, ''), COALESCE(discount_amount, 0), total, motoboy_id, motoboy_name, waiter_id, COALESCE(waiter_name, ''), cashier_id, COALESCE(cashier_name, ''), COALESCE(shift_id, 1), qr_payload, created_at, COALESCE(created_at_iso, ''), updated_at
		FROM orders`
	var args []any
	if statusFilter != "" {
		query += ` WHERE status = ?`
		args = append(args, statusFilter)
	}
	query += ` ORDER BY id DESC LIMIT 200`

	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cfg := s.GetPrinterConfig()
	var orders []models.Order
	for rows.Next() {
		var o models.Order
		var tableID, comandaID, motoboyID, waiterID, cashierID sql.NullInt64
		err := rows.Scan(
			&o.ID, &o.OrderCode, &o.OriginType, &o.HeaderTag, &tableID, &comandaID, &o.ComandaCode,
			&o.CustomerName, &o.CustomerPhone, &o.AddressText, &o.Latitude, &o.Longitude,
			&o.Status, &o.PaymentMethod, &o.GatewayID, &o.StatusPagamento, &o.ChangeFor,
			&o.Subtotal, &o.DeliveryFee, &o.CouponCode, &o.DiscountAmount, &o.Total, &motoboyID, &o.MotoboyName, &waiterID, &o.WaiterName, &cashierID, &o.CashierName, &o.ShiftID,
			&o.QRPayload, &o.CreatedAt, &o.CreatedAtISO, &o.UpdatedAt,
		)
		if err != nil {
			continue
		}
		if tableID.Valid {
			tid := int(tableID.Int64)
			o.TableID = &tid
		}
		if comandaID.Valid {
			cid := int(comandaID.Int64)
			o.ComandaID = &cid
		}
		if motoboyID.Valid {
			mid := int(motoboyID.Int64)
			o.MotoboyID = &mid
		}
		if waiterID.Valid && waiterID.Int64 > 0 {
			wid := int(waiterID.Int64)
			o.WaiterID = &wid
		}
		if cashierID.Valid && cashierID.Int64 > 0 {
			csh := int(cashierID.Int64)
			o.CashierID = &csh
		} else {
			defCsh := 2
			o.CashierID = &defCsh
		}
		o.Items = s.getOrderItems(o.ID)
		if strings.EqualFold(o.OriginType, "DELIVERY") {
			o.TrackingURL = fmt.Sprintf("https://sirbeefnup.github.io/rastreio?pedido=%s", o.OrderCode)
		}
		o.ReceiptPreview = escpos.RenderTextReceipt(o, cfg.Columns)
		o.SectorTickets = s.BuildOrderSectorTickets(o)
		orders = append(orders, o)
	}
	return orders, nil
}

func (s *Store) GetOrderByIDOrQR(idOrQR string) (*models.Order, error) {
	idOrQR = strings.TrimSpace(idOrQR)
	idOrQR = strings.TrimPrefix(idOrQR, "SIRBEEF:ORDER:")
	idOrQR = strings.TrimPrefix(idOrQR, "#")

	orders, err := s.ListOrders("")
	if err != nil {
		return nil, err
	}
	for _, o := range orders {
		if strconv.Itoa(o.ID) == idOrQR || strings.EqualFold(o.OrderCode, idOrQR) || strings.EqualFold(o.QRPayload, idOrQR) {
			return &o, nil
		}
	}
	return nil, fmt.Errorf("pedido %s não encontrado", idOrQR)
}

func (s *Store) getOrderItems(orderID int) []models.OrderItem {
	rows, err := s.DB.Query(`SELECT id, order_id, product_id, product_name, quantity, unit_price, total_price, selected_modifiers_json, notes, sector_abc
		FROM order_items WHERE order_id = ? ORDER BY id ASC`, orderID)
	if err != nil {
		return []models.OrderItem{}
	}
	defer rows.Close()
	items := []models.OrderItem{}
	for rows.Next() {
		var it models.OrderItem
		var modJSON string
		if err := rows.Scan(&it.ID, &it.OrderID, &it.ProductID, &it.ProductName, &it.Quantity, &it.UnitPrice, &it.TotalPrice, &modJSON, &it.Notes, &it.SectorABC); err == nil {
			_ = json.Unmarshal([]byte(modJSON), &it.SelectedModifiers)
			if it.SelectedModifiers == nil {
				it.SelectedModifiers = []models.SelectedModifier{}
			}
			items = append(items, it)
		}
	}
	return items
}

// UpdateOrderStatus atualiza o status no Kanban (avanço ➡️ ou retorno ⬅️), dispara notificação automática no WhatsApp
// exclusivamente para pedidos de DELIVERY com delay de 40 segundos (cancelado se a etapa retornar), e gerencia o acerto do Motoboy.
func (s *Store) UpdateOrderStatus(orderID int, newStatus string, motoboyID int, motoboyName string) (*models.Order, error) {
	newStatus = strings.ToUpper(strings.TrimSpace(newStatus))
	var oldStatus, oldPayStatus, oldPayMethod string
	_ = s.DB.QueryRow(`SELECT COALESCE(status,''), COALESCE(status_pagamento,''), COALESCE(payment_method,'') FROM orders WHERE id = ?`, orderID).Scan(&oldStatus, &oldPayStatus, &oldPayMethod)

	isReverting := orderStageRank(newStatus) < orderStageRank(oldStatus) && orderStageRank(newStatus) > 0

	// Cancela qualquer timer pendente de 40s do WhatsApp para este pedido
	pendingWhatsAppMu.Lock()
	if tm, exists := pendingWhatsAppTimers[orderID]; exists && tm != nil {
		tm.Stop()
		delete(pendingWhatsAppTimers, orderID)
	}
	pendingWhatsAppMu.Unlock()

	now := time.Now().Format("02/01 15:04")
	if motoboyID > 0 {
		if motoboyName == "" {
			_ = s.DB.QueryRow(`SELECT name FROM users WHERE id = ?`, motoboyID).Scan(&motoboyName)
		}
		_, err := s.DB.Exec(`UPDATE orders SET status = ?, motoboy_id = ?, motoboy_name = ?, updated_at = ? WHERE id = ?`,
			newStatus, motoboyID, motoboyName, now, orderID)
		if err != nil {
			return nil, err
		}
	} else if isReverting && (newStatus == "RECEBIDO" || newStatus == "PREPARANDO" || newStatus == "PRONTO") {
		// Se retornou para antes de DESPACHADO, limpa vínculo de motoboy e estorna lançamentos do acerto daquele pedido
		_, err := s.DB.Exec(`UPDATE orders SET status = ?, motoboy_id = NULL, motoboy_name = '', updated_at = ? WHERE id = ?`,
			newStatus, now, orderID)
		if err != nil {
			return nil, err
		}
		_, _ = s.DB.Exec(`DELETE FROM motoboy_ledger WHERE order_id = ?`, orderID)
	} else {
		_, err := s.DB.Exec(`UPDATE orders SET status = ?, updated_at = ? WHERE id = ?`,
			newStatus, now, orderID)
		if err != nil {
			return nil, err
		}
	}

	if isReverting && strings.EqualFold(oldStatus, "ENTREGUE") && !strings.EqualFold(newStatus, "ENTREGUE") {
		if !strings.EqualFold(oldPayMethod, "PIX_ONLINE") {
			_, _ = s.DB.Exec(`UPDATE orders SET status_pagamento = 'PENDENTE' WHERE id = ?`, orderID)
		}
	}

	ord, err := s.GetOrderByIDOrQR(strconv.Itoa(orderID))
	if err != nil {
		return nil, err
	}

	// Se foi DESPACHADO, processa a carteira do Motoboy (Fase 1 Dinheiro na Entrega vs Fase 2 Online)
	if newStatus == "DESPACHADO" && ord.MotoboyID != nil && *ord.MotoboyID > 0 {
		var existing int
		_ = s.DB.QueryRow(`SELECT COUNT(*) FROM motoboy_ledger WHERE order_id = ?`, ord.ID).Scan(&existing)
		if existing == 0 {
			// Credita a taxa de entrega para o motoboy
			if ord.DeliveryFee > 0 {
				_, _ = s.DB.Exec(`INSERT INTO motoboy_ledger (motoboy_id, motoboy_name, order_id, entry_type, amount, description, created_at)
					VALUES (?, ?, ?, 'CREDITO_TAXA_ENTREGA', ?, ?, ?)`,
					*ord.MotoboyID, ord.MotoboyName, ord.ID, ord.DeliveryFee,
					fmt.Sprintf("Taxa de entrega Pedido #%s", ord.OrderCode), now)
			}
			// Se pagamento for DINHEIRO na entrega (Fase 1), debita o valor total que o cliente pagou em mãos ao motoboy
			if ord.PaymentMethod == "DINHEIRO" && ord.StatusPagamento == "PENDENTE" {
				_, _ = s.DB.Exec(`INSERT INTO motoboy_ledger (motoboy_id, motoboy_name, order_id, entry_type, amount, description, created_at)
					VALUES (?, ?, ?, 'DEBITO_DINHEIRO_PEDIDO', ?, ?, ?)`,
					*ord.MotoboyID, ord.MotoboyName, ord.ID, -ord.Total,
					fmt.Sprintf("Recebimento em Dinheiro Pedido #%s", ord.OrderCode), now)
			}
		}
	}

	if newStatus == "ENTREGUE" {
		_, _ = s.DB.Exec(`UPDATE orders SET status_pagamento = 'PAGO' WHERE id = ?`, ord.ID)
		ord.StatusPagamento = "PAGO"
	}
	if newStatus == "CANCELADO" || oldStatus == "CANCELADO" {
		cid := 0
		if ord.ComandaID != nil {
			cid = *ord.ComandaID
		}
		tid := 0
		if ord.TableID != nil {
			tid = *ord.TableID
		}
		if cid > 0 || tid > 0 {
			s.closeEmptyComandaAndRecalcTable(cid, tid)
		}
	}
	s.RecalcCashierShiftTotals()

	if isReverting {
		_ = s.AddSystemLog("WARN", "EXPEDICAO-RETORNO", fmt.Sprintf("⬅️ Pedido #%s (%s) retornado de %s para %s. Qualquer envio pendente de WhatsApp (delay 40s) foi cancelado.", ord.OrderCode, ord.OriginType, oldStatus, newStatus), "", "EXPEDICAO")
		return ord, nil
	}

	_ = s.AddSystemLog("INFO", "EXPEDICAO-AVANCO", fmt.Sprintf("➡️ Pedido #%s (%s) avançou de %s para %s.", ord.OrderCode, ord.OriginType, oldStatus, newStatus), "", "EXPEDICAO")

	// Dispara notificação automática no WhatsApp APENAS para pedidos DELIVERY e com DELAY de 40 SEGUNDOS!
	// Se o operador retornar a etapa dentro dos 40s, o timer é cancelado e a mensagem não é enviada.
	if strings.EqualFold(ord.OriginType, "DELIVERY") && strings.TrimSpace(ord.CustomerPhone) != "" && newStatus != oldStatus {
		targetOrderID := ord.ID
		targetStatus := newStatus
		_ = s.AddSystemLog("INFO", "WHATSAPP-DELAY-40S", fmt.Sprintf("⏳ Agendado aviso WhatsApp (40s) para Pedido Delivery #%s -> etapa %s (Cliente: %s).", ord.OrderCode, targetStatus, ord.CustomerName), "", "WHATSAPP-BOT")
		pendingWhatsAppMu.Lock()
		pendingWhatsAppTimers[targetOrderID] = time.AfterFunc(40*time.Second, func() {
			pendingWhatsAppMu.Lock()
			delete(pendingWhatsAppTimers, targetOrderID)
			pendingWhatsAppMu.Unlock()

			latestOrd, errLatest := s.GetOrderByIDOrQR(strconv.Itoa(targetOrderID))
			if errLatest != nil || latestOrd == nil {
				return
			}
			// Confirma que o pedido ainda está na etapa avançada e é DELIVERY
			if !strings.EqualFold(latestOrd.OriginType, "DELIVERY") || !strings.EqualFold(latestOrd.Status, targetStatus) {
				return
			}
			if strings.TrimSpace(latestOrd.CustomerPhone) != "" {
				msg := whatsapp.BuildOrderStatusMessage(*latestOrd)
				s.SendAndLogWhatsApp(latestOrd.CustomerPhone, latestOrd.CustomerName, "STATUS_"+targetStatus, msg)
			}
		})
		pendingWhatsAppMu.Unlock()
	}

	return ord, nil
}

func (s *Store) GetMotoboySettlement(motoboyID int) (*models.MotoboySettlementSummary, error) {
	if motoboyID <= 0 {
		motoboyID = 4 // Default Rafael Motoboy
	}
	var name string
	_ = s.DB.QueryRow(`SELECT name FROM users WHERE id = ?`, motoboyID).Scan(&name)
	if name == "" {
		name = "Rafael (Motoboy Express 01)"
	}

	rows, err := s.DB.Query(`SELECT id, motoboy_id, motoboy_name, order_id, entry_type, amount, description, created_at
		FROM motoboy_ledger WHERE motoboy_id = ? ORDER BY id DESC`, motoboyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	summary := &models.MotoboySettlementSummary{
		MotoboyID:   motoboyID,
		MotoboyName: name,
		Entries:     []models.MotoboyLedgerEntry{},
	}

	for rows.Next() {
		var e models.MotoboyLedgerEntry
		if err := rows.Scan(&e.ID, &e.MotoboyID, &e.MotoboyName, &e.OrderID, &e.EntryType, &e.Amount, &e.Description, &e.CreatedAt); err == nil {
			summary.Entries = append(summary.Entries, e)
			if e.EntryType == "CREDITO_TAXA_ENTREGA" {
				summary.TotalDeliveries++
				summary.DeliveryFeesEarned += e.Amount
			} else if e.EntryType == "DEBITO_DINHEIRO_PEDIDO" {
				summary.CashCollected += math.Abs(e.Amount)
			}
		}
	}
	// Quanto o motoboy deve entregar fisicamente ao caixa no acerto da noite (Dinheiro recebido - Taxas de entrega)
	summary.NetBalanceToHouse = summary.CashCollected - summary.DeliveryFeesEarned
	return summary, nil
}

func (s *Store) OpenMotoboyShift(motoboyID int, motoboyName string, openingFloat float64) (*models.MotoboyShift, error) {
	if motoboyID <= 0 {
		motoboyID = 4
	}
	if strings.TrimSpace(motoboyName) == "" {
		_ = s.DB.QueryRow(`SELECT name FROM users WHERE id = ?`, motoboyID).Scan(&motoboyName)
		if motoboyName == "" {
			motoboyName = fmt.Sprintf("Motoboy #%d", motoboyID)
		}
	}
	nowStr := time.Now().Format("02/01 15:04")
	_, err := s.DB.Exec(`INSERT INTO motoboy_shifts (motoboy_id, motoboy_name, status, opening_float, opened_at, closed_at)
		VALUES (?, ?, 'ABERTO', ?, ?, '')
		ON CONFLICT(motoboy_id) DO UPDATE SET motoboy_name=excluded.motoboy_name, status='ABERTO', opening_float=excluded.opening_float, opened_at=excluded.opened_at, closed_at=''`,
		motoboyID, motoboyName, openingFloat, nowStr)
	if err != nil {
		return nil, err
	}
	shifts, _ := s.ListMotoboyShifts()
	for _, sh := range shifts {
		if sh.MotoboyID == motoboyID {
			return &sh, nil
		}
	}
	return &models.MotoboyShift{
		MotoboyID:    motoboyID,
		MotoboyName:  motoboyName,
		Status:       "ABERTO",
		OpeningFloat: openingFloat,
		OpenedAt:     nowStr,
		OrdersList:   []models.Order{},
	}, nil
}

func (s *Store) CloseMotoboyShift(motoboyID int) (*models.MotoboyShift, error) {
	nowStr := time.Now().Format("02/01 15:04")
	_, err := s.DB.Exec(`UPDATE motoboy_shifts SET status = 'FECHADO', closed_at = ? WHERE motoboy_id = ?`, nowStr, motoboyID)
	if err != nil {
		return nil, err
	}
	shifts, _ := s.ListMotoboyShifts()
	for _, sh := range shifts {
		if sh.MotoboyID == motoboyID {
			return &sh, nil
		}
	}
	return nil, nil
}

func (s *Store) ListMotoboyShifts() ([]models.MotoboyShift, error) {
	// Remove turnos órfãos de usuários que foram excluídos pelo ADM
	_, _ = s.DB.Exec(`DELETE FROM motoboy_shifts WHERE motoboy_id NOT IN (SELECT id FROM users)`)
	// Garante que todos os usuários com cargo MOTOBOY tenham registro na tabela de caixas individuais
	users, _ := s.ListUsers()
	for _, u := range users {
		if strings.EqualFold(u.Role, "MOTOBOY") && u.Active {
			_, _ = s.DB.Exec(`INSERT OR IGNORE INTO motoboy_shifts (motoboy_id, motoboy_name, status, opening_float, opened_at, closed_at) VALUES (?, ?, 'FECHADO', 0, '', '')`,
				u.ID, u.Name)
		}
	}

	rows, err := s.DB.Query(`SELECT id, motoboy_id, motoboy_name, status, opening_float, opened_at, closed_at FROM motoboy_shifts ORDER BY CASE WHEN status = 'ABERTO' THEN 0 ELSE 1 END, motoboy_id ASC`)
	if err != nil {
		return []models.MotoboyShift{}, err
	}
	defer rows.Close()

	allOrders, _ := s.ListOrders("")
	res := []models.MotoboyShift{}
	for rows.Next() {
		var sh models.MotoboyShift
		if err := rows.Scan(&sh.ID, &sh.MotoboyID, &sh.MotoboyName, &sh.Status, &sh.OpeningFloat, &sh.OpenedAt, &sh.ClosedAt); err == nil {
			sh.OrdersList = []models.Order{}
			for _, ord := range allOrders {
				if !strings.EqualFold(ord.OriginType, "DELIVERY") {
					continue
				}
				matchedMoto := (ord.MotoboyID != nil && *ord.MotoboyID == sh.MotoboyID) ||
					(ord.MotoboyID == nil && strings.EqualFold(strings.TrimSpace(ord.MotoboyName), strings.TrimSpace(sh.MotoboyName)))
				if matchedMoto && (ord.Status == "DESPACHADO" || ord.Status == "ENTREGUE") {
					sh.DeliveriesCount++
					sh.FeesEarned += ord.DeliveryFee
					if ord.PaymentMethod == "DINHEIRO" {
						sh.CashOrdersTotal += ord.Total
					} else {
						sh.CardPixTotal += ord.Total
					}
					sh.OrdersList = append(sh.OrdersList, ord)
				}
			}
			sh.NetToReturn = (sh.OpeningFloat + sh.CashOrdersTotal) - sh.FeesEarned
			if sh.NetToReturn < 0 {
				sh.NetToReturn = 0
			}
			res = append(res, sh)
		}
	}
	return res, nil
}

func (s *Store) LogWhatsApp(phone, customerName, direction, trigger, message string) {
	s.LogWhatsAppWithStatus(phone, customerName, direction, trigger, message, "ENVIADO")
}

func (s *Store) LogWhatsAppWithStatus(phone, customerName, direction, trigger, message, status string) {
	if status == "" {
		status = "ENVIADO"
	}
	now := time.Now().Format("02/01 15:04:05")
	_, _ = s.DB.Exec(`INSERT INTO whatsapp_logs (phone, customer_name, direction, trigger_event, message_text, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, phone, customerName, direction, trigger, message, status, now)
}

func (s *Store) SendAndLogWhatsApp(phone, customerName, trigger, message string) {
	cleanDigits := whatsapp.NormalizeWhatsAppDigits(phone)
	if len(cleanDigits) < 10 {
		s.LogWhatsAppWithStatus(phone, customerName, "OUTBOUND", trigger, message, "SEM_TELEFONE")
		return
	}
	if s.WhatsApp != nil && s.WhatsApp.IsAutoOrderNotifyEnabled() && s.WhatsApp.GetStatus().Connected {
		go func(p, c, t, m string) {
			if err := s.WhatsApp.SendText(p, m); err != nil {
				s.LogWhatsAppWithStatus(p, c, "OUTBOUND", t, m, "ERRO_ENVIO")
			} else {
				s.LogWhatsAppWithStatus(p, c, "OUTBOUND", t, m, "ENVIADO_WHATSAPP")
			}
		}(phone, customerName, trigger, message)
		return
	}
	s.LogWhatsAppWithStatus(phone, customerName, "OUTBOUND", trigger, message, "REGISTRADO_PDV")
}

func (s *Store) ListWhatsAppLogs() ([]models.WhatsAppMessageLog, error) {
	rows, err := s.DB.Query(`SELECT id, phone, customer_name, direction, trigger_event, message_text, status, created_at FROM whatsapp_logs ORDER BY id DESC LIMIT 150`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []models.WhatsAppMessageLog
	for rows.Next() {
		var l models.WhatsAppMessageLog
		if err := rows.Scan(&l.ID, &l.Phone, &l.CustomerName, &l.Direction, &l.TriggerEvent, &l.MessageText, &l.Status, &l.CreatedAt); err == nil {
			list = append(list, l)
		}
	}
	if list == nil {
		list = []models.WhatsAppMessageLog{}
	}
	return list, nil
}

func (s *Store) DeleteWhatsAppLog(id int) error {
	if id <= 0 {
		return fmt.Errorf("ID inválido")
	}
	_, err := s.DB.Exec(`DELETE FROM whatsapp_logs WHERE id = ?`, id)
	return err
}

func (s *Store) ClearAllWhatsAppLogs() error {
	_, err := s.DB.Exec(`DELETE FROM whatsapp_logs`)
	return err
}

func (s *Store) AddSystemLog(level, module, message, stackTrace, deviceInfo string) error {
	now := time.Now().Format("02/01/2006 15:04:05")
	_, err := s.DB.Exec(`INSERT INTO system_logs (level, module, message, stack_trace, device_info, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`, level, module, message, stackTrace, deviceInfo, now)
	return err
}

func (s *Store) ListSystemLogs() ([]models.SystemLog, error) {
	rows, err := s.DB.Query(`SELECT id, level, module, message, stack_trace, device_info, created_at FROM system_logs ORDER BY id DESC LIMIT 300`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []models.SystemLog
	for rows.Next() {
		var l models.SystemLog
		if err := rows.Scan(&l.ID, &l.Level, &l.Module, &l.Message, &l.StackTrace, &l.DeviceInfo, &l.CreatedAt); err == nil {
			list = append(list, l)
		}
	}
	return list, nil
}

func DefaultOperatingHoursConfig() models.OperatingHoursConfig {
	return models.OperatingHoursConfig{
		AutoScheduleEnabled: false,
		ManualOverrideMode:  "AUTO",
		Days: []models.DayOperatingHours{
			{DayOfWeek: 1, DayName: "Segunda-feira", Enabled: false, IsOpen: false, OpenTime: "18:00", CloseTime: "23:30"},
			{DayOfWeek: 2, DayName: "Terça-feira", Enabled: true, IsOpen: true, OpenTime: "18:00", CloseTime: "23:30"},
			{DayOfWeek: 3, DayName: "Quarta-feira", Enabled: true, IsOpen: true, OpenTime: "18:00", CloseTime: "23:30"},
			{DayOfWeek: 4, DayName: "Quinta-feira", Enabled: true, IsOpen: true, OpenTime: "18:00", CloseTime: "23:30"},
			{DayOfWeek: 5, DayName: "Sexta-feira", Enabled: true, IsOpen: true, OpenTime: "18:00", CloseTime: "23:59"},
			{DayOfWeek: 6, DayName: "Sábado", Enabled: true, IsOpen: true, OpenTime: "18:00", CloseTime: "23:59"},
			{DayOfWeek: 0, DayName: "Domingo", Enabled: true, IsOpen: true, OpenTime: "18:00", CloseTime: "23:30"},
		},
	}
}

func parseTimeMinutes(hhmm string, defaultMin int) int {
	parts := strings.Split(strings.TrimSpace(hhmm), ":")
	if len(parts) < 2 {
		return defaultMin
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return defaultMin
	}
	return h*60 + m
}

func EvaluateOperatingHours(oh models.OperatingHoursConfig, now time.Time) (bool, string, string) {
	if len(oh.Days) == 0 {
		oh = DefaultOperatingHoursConfig()
	}
	for i := range oh.Days {
		if oh.Days[i].Enabled || oh.Days[i].IsOpen {
			oh.Days[i].Enabled = true
			oh.Days[i].IsOpen = true
		}
	}
	// Weekday(): Sunday=0, Monday=1..Saturday=6
	wd := int(now.Weekday())
	prevWd := (wd + 6) % 7

	matchDay := func(cfgDow, targetDow int) bool {
		if targetDow == 0 {
			return cfgDow == 0 || cfgDow == 7
		}
		return cfgDow == targetDow
	}

	var today, yesterday models.DayOperatingHours
	for _, d := range oh.Days {
		if matchDay(d.DayOfWeek, wd) {
			today = d
		}
		if matchDay(d.DayOfWeek, prevWd) {
			yesterday = d
		}
	}
	if today.DayName == "" {
		today = models.DayOperatingHours{DayOfWeek: wd, DayName: "Hoje", Enabled: true, IsOpen: true, OpenTime: "18:00", CloseTime: "23:59"}
	}
	dayOpen := today.Enabled || today.IsOpen
	prevOpen := yesterday.Enabled || yesterday.IsOpen

	todayLabel := fmt.Sprintf("%s: Fechado", today.DayName)
	if dayOpen {
		todayLabel = fmt.Sprintf("%s: %s às %s", today.DayName, today.OpenTime, today.CloseTime)
	}

	mode := strings.ToUpper(strings.TrimSpace(oh.ManualOverrideMode))
	if mode == "FORCE_OPEN" {
		return true, todayLabel + " (Aberto Manualmente)", ""
	}
	if mode == "FORCE_CLOSED" {
		return false, todayLabel, "🔴 Loja fechada temporariamente pela gerência. Confira nossos horários de funcionamento!"
	}
	if !oh.AutoScheduleEnabled {
		return true, todayLabel, ""
	}

	nowMin := now.Hour()*60 + now.Minute()
	// Verifica se ainda estamos na madrugada do turno de ontem (ex: 18:00 às 01:30)
	if prevOpen && yesterday.OpenTime != "" && yesterday.CloseTime != "" {
		yOpen := parseTimeMinutes(yesterday.OpenTime, 18*60)
		yClose := parseTimeMinutes(yesterday.CloseTime, 23*60+59)
		if yClose < yOpen && nowMin <= yClose {
			return true, fmt.Sprintf("%s: %s às %s", yesterday.DayName, yesterday.OpenTime, yesterday.CloseTime), ""
		}
	}

	if !dayOpen {
		return false, todayLabel, fmt.Sprintf("🔴 Estamos fechados hoje (%s). Confira nossos dias e horários de funcionamento!", today.DayName)
	}

	openMin := parseTimeMinutes(today.OpenTime, 8*60)
	closeMin := parseTimeMinutes(today.CloseTime, 23*60+59)
	isOpen := false
	if closeMin >= openMin {
		isOpen = nowMin >= openMin && nowMin <= closeMin
	} else {
		// Virada de meia-noite (ex: 18:00 às 01:30)
		isOpen = nowMin >= openMin || nowMin <= closeMin
	}
	if !isOpen {
		return false, todayLabel, fmt.Sprintf("🕒 Estamos fechados no momento! Hoje (%s) nosso horário de funcionamento é das %s às %s.", today.DayName, today.OpenTime, today.CloseTime)
	}
	return true, todayLabel, ""
}

func isBoutiqueOrMarketCategory(catName, sector, sectorABC string) bool {
	cLower := strings.ToLower(strings.TrimSpace(catName))
	sLower := strings.ToLower(strings.TrimSpace(sector))
	abcLower := strings.ToLower(strings.TrimSpace(sectorABC))
	return strings.Contains(cLower, "boutique") ||
		strings.Contains(cLower, "empório") ||
		strings.Contains(cLower, "emporio") ||
		strings.Contains(cLower, "mercado") ||
		strings.Contains(cLower, "açougue") ||
		strings.Contains(cLower, "acougue") ||
		sLower == "mercado" ||
		abcLower == "mercado_carnes"
}

type aiNFCeStoredSettings struct {
	AIOrderBotEnabled     bool   `json:"ai_order_bot_enabled"`
	AIProvider            string `json:"ai_provider"`
	AICanCreateOrders     bool   `json:"ai_can_create_orders"`
	AIIncludeBoutique     bool   `json:"ai_include_boutique"`
	AICustomInstructions  string `json:"ai_custom_instructions"`
	NFCeEnabled           bool   `json:"nfce_enabled"`
	NFCeAmbiente          string `json:"nfce_ambiente"`
	CNPJEmitente          string `json:"cnpj_emitente"`
	NFCeRazaoSocial       string `json:"nfce_razao_social"`
	NFCeInscricaoEstadual string `json:"nfce_inscricao_estadual"`
	NFCeRegimeTributario  string `json:"nfce_regime_tributario"`
	NFCeEnderecoFiscal    string `json:"nfce_endereco_fiscal"`
	NFCeSerie             int    `json:"nfce_serie"`
	NFCeProximoNumero     int    `json:"nfce_proximo_numero"`
	NFCeCSCID             string `json:"nfce_csc_id"`
	NFCeCSCToken          string `json:"nfce_csc_token"`
	NFCeCertFilename      string `json:"nfce_cert_filename"`
	NFCeCertBase64        string `json:"nfce_cert_base64"`
	NFCeCertPassword      string `json:"nfce_cert_password"`
	NFCeAPIToken          string `json:"nfce_api_token"`
}

func (s *Store) GetPrinterConfig() models.PrinterConfig {
	var cfg models.PrinterConfig
	var autoPrint, alarmTable, alarmDrinks, alarmKitchen, menuDel, menuRet, menuMesa, menuKiosk, allowStaffDelivFee int
	var defDelivFee float64
	var alarmMode, waiterVisJSON, waiterHidJSON, opHoursJSON, aiNFCeJSON string
	err := s.DB.QueryRow(`SELECT
		columns, paper_width_mm, printer_ip, printer_port, auto_print_on_accept, whatsapp_bot_phase, gemini_api_key,
		COALESCE(waiter_alarm_table_call, 1), COALESCE(waiter_alarm_drinks, 1), COALESCE(waiter_alarm_kitchen_ready, 1),
		COALESCE(waiter_alarm_mode, 'TOCAR_E_VIBRAR'),
		COALESCE(menu_enable_delivery, 1), COALESCE(menu_enable_retirada, 1), COALESCE(menu_enable_mesa, 1), COALESCE(menu_enable_kiosk, 1),
		COALESCE(waiter_visible_categories_json, '[]'), COALESCE(waiter_hidden_categories_json, '["Boutique das Carnes & Empório"]'),
		COALESCE(default_delivery_fee, 8.0), COALESCE(allow_staff_edit_delivery_fee, 1),
		COALESCE(operating_hours_json, ''), COALESCE(ai_nfce_settings_json, '{}')
		FROM printer_config WHERE id = 1`).
		Scan(
			&cfg.Columns, &cfg.PaperWidthMM, &cfg.PrinterIP, &cfg.PrinterPort, &autoPrint, &cfg.WhatsappBotPhase, &cfg.GeminiAPIKey,
			&alarmTable, &alarmDrinks, &alarmKitchen, &alarmMode, &menuDel, &menuRet, &menuMesa, &menuKiosk,
			&waiterVisJSON, &waiterHidJSON, &defDelivFee, &allowStaffDelivFee, &opHoursJSON, &aiNFCeJSON,
		)
	if err != nil {
		defOH := DefaultOperatingHoursConfig()
		isOpen, todayLbl, closedMsg := EvaluateOperatingHours(defOH, time.Now())
		return models.PrinterConfig{
			Columns:                   48,
			PaperWidthMM:              80,
			PrinterIP:                 "192.168.1.200",
			PrinterPort:               9100,
			AutoPrintOnAccept:         true,
			WhatsappBotPhase:          1,
			AIOrderBotEnabled:         false,
			AIProvider:                "GEMINI_2_5_FLASH",
			AICanCreateOrders:         true,
			AICustomInstructions:      "Atenda com simpatia na Hamburgueria & Boutique Sir Beef. Pergunte o ponto da carne nos burgers, ofereça adicionais e confirme se é Entrega (Delivery) ou Retirada no Balcão.",
			NFCeEnabled:               false,
			NFCeAmbiente:              "HOMOLOGACAO",
			NFCeRegimeTributario:      "SIMPLES_NACIONAL",
			NFCeSerie:                 1,
			NFCeProximoNumero:         1,
			NFCeCSCID:                 "000001",
			WaiterAlarmTableCall:      true,
			WaiterAlarmDrinks:         true,
			WaiterAlarmKitchenReady:   true,
			WaiterAlarmMode:           "TOCAR_E_VIBRAR",
			MenuEnableDelivery:        true,
			MenuEnableRetirada:        true,
			MenuEnableMesa:            true,
			MenuEnableKiosk:           true,
			WaiterVisibleCategories:   []int{},
			WaiterHiddenCategories:    []int{900},
			DefaultDeliveryFee:        8.0,
			AllowStaffEditDeliveryFee: true,
			OperatingHours:            defOH,
			IsOpenNow:                 isOpen,
			TodayHoursLabel:           todayLbl,
			ClosedReasonMessage:       closedMsg,
		}
	}
	cfg.AutoPrintOnAccept = autoPrint == 1
	cfg.WaiterAlarmTableCall = alarmTable == 1
	cfg.WaiterAlarmDrinks = alarmDrinks == 1
	cfg.WaiterAlarmKitchenReady = alarmKitchen == 1
	if alarmMode != "SO_VIBRAR" {
		alarmMode = "TOCAR_E_VIBRAR"
	}
	cfg.WaiterAlarmMode = alarmMode
	cfg.MenuEnableDelivery = menuDel == 1
	cfg.MenuEnableRetirada = menuRet == 1
	cfg.MenuEnableMesa = menuMesa == 1
	cfg.MenuEnableKiosk = menuKiosk == 1
	if defDelivFee < 0 {
		defDelivFee = 0
	}
	cfg.DefaultDeliveryFee = defDelivFee
	cfg.AllowStaffEditDeliveryFee = allowStaffDelivFee == 1

	// Valores padrão para os módulos preparados para o futuro (desativados por padrão)
	extra := aiNFCeStoredSettings{
		AIOrderBotEnabled:    cfg.WhatsappBotPhase == 2,
		AIProvider:           "GEMINI_2_5_FLASH",
		AICanCreateOrders:    true,
		AICustomInstructions: "Atenda com simpatia na Hamburgueria & Boutique Sir Beef. Pergunte o ponto da carne nos burgers, ofereça adicionais e confirme se é Entrega (Delivery) ou Retirada no Balcão.",
		NFCeEnabled:          false,
		NFCeAmbiente:         "HOMOLOGACAO",
		NFCeRegimeTributario: "SIMPLES_NACIONAL",
		NFCeSerie:            1,
		NFCeProximoNumero:    1,
		NFCeCSCID:            "000001",
	}
	if strings.TrimSpace(aiNFCeJSON) != "" && strings.TrimSpace(aiNFCeJSON) != "{}" {
		_ = json.Unmarshal([]byte(aiNFCeJSON), &extra)
	}
	if strings.TrimSpace(extra.AIProvider) == "" {
		extra.AIProvider = "GEMINI_2_5_FLASH"
	}
	if strings.TrimSpace(extra.NFCeAmbiente) == "" {
		extra.NFCeAmbiente = "HOMOLOGACAO"
	}
	if strings.TrimSpace(extra.NFCeRegimeTributario) == "" {
		extra.NFCeRegimeTributario = "SIMPLES_NACIONAL"
	}
	if extra.NFCeSerie <= 0 {
		extra.NFCeSerie = 1
	}
	if extra.NFCeProximoNumero <= 0 {
		extra.NFCeProximoNumero = 1
	}
	if strings.TrimSpace(extra.NFCeCSCID) == "" {
		extra.NFCeCSCID = "000001"
	}
	cfg.AIOrderBotEnabled = extra.AIOrderBotEnabled
	if cfg.AIOrderBotEnabled {
		cfg.WhatsappBotPhase = 2
	} else {
		cfg.WhatsappBotPhase = 1
	}
	cfg.AIProvider = extra.AIProvider
	cfg.AICanCreateOrders = extra.AICanCreateOrders
	cfg.AIIncludeBoutique = extra.AIIncludeBoutique
	cfg.AICustomInstructions = extra.AICustomInstructions
	cfg.NFCeEnabled = extra.NFCeEnabled
	cfg.NFCeAmbiente = extra.NFCeAmbiente
	cfg.CNPJEmitente = extra.CNPJEmitente
	cfg.NFCeRazaoSocial = extra.NFCeRazaoSocial
	cfg.NFCeInscricaoEstadual = extra.NFCeInscricaoEstadual
	cfg.NFCeRegimeTributario = extra.NFCeRegimeTributario
	cfg.NFCeEnderecoFiscal = extra.NFCeEnderecoFiscal
	cfg.NFCeSerie = extra.NFCeSerie
	cfg.NFCeProximoNumero = extra.NFCeProximoNumero
	cfg.NFCeCSCID = extra.NFCeCSCID
	cfg.NFCeCSCToken = extra.NFCeCSCToken
	cfg.NFCeCertFilename = extra.NFCeCertFilename
	cfg.NFCeCertBase64 = extra.NFCeCertBase64
	cfg.NFCeCertPassword = extra.NFCeCertPassword
	cfg.NFCeAPIToken = extra.NFCeAPIToken

	_ = json.Unmarshal([]byte(waiterVisJSON), &cfg.WaiterVisibleCategories)
	_ = json.Unmarshal([]byte(waiterHidJSON), &cfg.WaiterHiddenCategories)
	if len(cfg.WaiterHiddenCategories) == 0 && len(cfg.WaiterVisibleCategories) == 0 {
		cfg.WaiterHiddenCategories = []int{900}
	}
	if len(cfg.WaiterVisibleCategories) == 0 {
		cats, _ := s.GetCatalogAll(true)
		hiddenSet := map[int]bool{}
		for _, h := range cfg.WaiterHiddenCategories {
			hiddenSet[h] = true
		}
		for _, c := range cats {
			if hiddenSet[c.ID] || isBoutiqueOrMarketCategory(c.Name, c.Sector, c.SectorABC) {
				if !hiddenSet[c.ID] {
					cfg.WaiterHiddenCategories = append(cfg.WaiterHiddenCategories, c.ID)
					hiddenSet[c.ID] = true
				}
				continue
			}
			cfg.WaiterVisibleCategories = append(cfg.WaiterVisibleCategories, c.ID)
		}
	}

	var oh models.OperatingHoursConfig
	if strings.TrimSpace(opHoursJSON) == "" || json.Unmarshal([]byte(opHoursJSON), &oh) != nil || len(oh.Days) == 0 {
		oh = DefaultOperatingHoursConfig()
	}
	if strings.TrimSpace(oh.ManualOverrideMode) == "" {
		oh.ManualOverrideMode = "AUTO"
	}
	cfg.OperatingHours = oh
	cfg.IsOpenNow, cfg.TodayHoursLabel, cfg.ClosedReasonMessage = EvaluateOperatingHours(oh, time.Now())
	return cfg
}

func (s *Store) UpdatePrinterConfig(cfg models.PrinterConfig) error {
	if cfg.Columns != 32 && cfg.Columns != 48 {
		cfg.Columns = 48
	}
	if cfg.Columns == 32 {
		cfg.PaperWidthMM = 58
	} else {
		cfg.PaperWidthMM = 80
	}
	auto := 0
	if cfg.AutoPrintOnAccept {
		auto = 1
	}
	if cfg.AIOrderBotEnabled || cfg.WhatsappBotPhase == 2 {
		cfg.AIOrderBotEnabled = true
		cfg.WhatsappBotPhase = 2
	} else {
		cfg.AIOrderBotEnabled = false
		cfg.WhatsappBotPhase = 1
	}
	alTable := 0
	if cfg.WaiterAlarmTableCall {
		alTable = 1
	}
	alDrinks := 0
	if cfg.WaiterAlarmDrinks {
		alDrinks = 1
	}
	alKitchen := 0
	if cfg.WaiterAlarmKitchenReady {
		alKitchen = 1
	}
	alMode := strings.ToUpper(strings.TrimSpace(cfg.WaiterAlarmMode))
	if alMode != "SO_VIBRAR" {
		alMode = "TOCAR_E_VIBRAR"
	}
	mDel := 0
	if cfg.MenuEnableDelivery {
		mDel = 1
	}
	mRet := 0
	if cfg.MenuEnableRetirada {
		mRet = 1
	}
	mMesa := 0
	if cfg.MenuEnableMesa {
		mMesa = 1
	}
	mKiosk := 0
	if cfg.MenuEnableKiosk {
		mKiosk = 1
	}
	if cfg.WaiterVisibleCategories == nil {
		cfg.WaiterVisibleCategories = []int{}
	}
	if cfg.WaiterHiddenCategories == nil {
		cfg.WaiterHiddenCategories = []int{900}
	}
	if len(cfg.OperatingHours.Days) == 0 {
		cfg.OperatingHours = DefaultOperatingHoursConfig()
	}
	visBytes, _ := json.Marshal(cfg.WaiterVisibleCategories)
	hidBytes, _ := json.Marshal(cfg.WaiterHiddenCategories)
	ohBytes, _ := json.Marshal(cfg.OperatingHours)
	if cfg.DefaultDeliveryFee < 0 {
		cfg.DefaultDeliveryFee = 0
	}
	allowStaffFee := 0
	if cfg.AllowStaffEditDeliveryFee {
		allowStaffFee = 1
	}

	if strings.TrimSpace(cfg.AIProvider) == "" {
		cfg.AIProvider = "GEMINI_2_5_FLASH"
	}
	if strings.TrimSpace(cfg.NFCeAmbiente) == "" {
		cfg.NFCeAmbiente = "HOMOLOGACAO"
	}
	if strings.TrimSpace(cfg.NFCeRegimeTributario) == "" {
		cfg.NFCeRegimeTributario = "SIMPLES_NACIONAL"
	}
	if cfg.NFCeSerie <= 0 {
		cfg.NFCeSerie = 1
	}
	if cfg.NFCeProximoNumero <= 0 {
		cfg.NFCeProximoNumero = 1
	}
	if strings.TrimSpace(cfg.NFCeCSCID) == "" {
		cfg.NFCeCSCID = "000001"
	}
	aiNFCeBytes, _ := json.Marshal(aiNFCeStoredSettings{
		AIOrderBotEnabled:     cfg.AIOrderBotEnabled,
		AIProvider:            cfg.AIProvider,
		AICanCreateOrders:     cfg.AICanCreateOrders,
		AIIncludeBoutique:     cfg.AIIncludeBoutique,
		AICustomInstructions:  cfg.AICustomInstructions,
		NFCeEnabled:           cfg.NFCeEnabled,
		NFCeAmbiente:          cfg.NFCeAmbiente,
		CNPJEmitente:          cfg.CNPJEmitente,
		NFCeRazaoSocial:       cfg.NFCeRazaoSocial,
		NFCeInscricaoEstadual: cfg.NFCeInscricaoEstadual,
		NFCeRegimeTributario:  cfg.NFCeRegimeTributario,
		NFCeEnderecoFiscal:    cfg.NFCeEnderecoFiscal,
		NFCeSerie:             cfg.NFCeSerie,
		NFCeProximoNumero:     cfg.NFCeProximoNumero,
		NFCeCSCID:             cfg.NFCeCSCID,
		NFCeCSCToken:          cfg.NFCeCSCToken,
		NFCeCertFilename:      cfg.NFCeCertFilename,
		NFCeCertBase64:        cfg.NFCeCertBase64,
		NFCeCertPassword:      cfg.NFCeCertPassword,
		NFCeAPIToken:          cfg.NFCeAPIToken,
	})

	_, err := s.DB.Exec(`UPDATE printer_config SET
		columns=?, paper_width_mm=?, printer_ip=?, printer_port=?, auto_print_on_accept=?, whatsapp_bot_phase=?, gemini_api_key=?,
		waiter_alarm_table_call=?, waiter_alarm_drinks=?, waiter_alarm_kitchen_ready=?, waiter_alarm_mode=?,
		menu_enable_delivery=?, menu_enable_retirada=?, menu_enable_mesa=?, menu_enable_kiosk=?,
		waiter_visible_categories_json=?, waiter_hidden_categories_json=?,
		default_delivery_fee=?, allow_staff_edit_delivery_fee=?, operating_hours_json=?, ai_nfce_settings_json=?
		WHERE id=1`,
		cfg.Columns, cfg.PaperWidthMM, cfg.PrinterIP, cfg.PrinterPort, auto, cfg.WhatsappBotPhase, cfg.GeminiAPIKey,
		alTable, alDrinks, alKitchen, alMode, mDel, mRet, mMesa, mKiosk,
		string(visBytes), string(hidBytes), cfg.DefaultDeliveryFee, allowStaffFee, string(ohBytes), string(aiNFCeBytes))
	return err
}

// GetFinanceDashboard calcula o DRE gerencial e a Curva ABC separando Hamburgueria vs Boutique das Carnes / Mercado.
func (s *Store) GetFinanceDashboard() (*models.FinanceDashboard, error) {
	orders, err := s.ListOrders("")
	if err != nil {
		return nil, err
	}

	dash := &models.FinanceDashboard{}
	burgerMap := map[int]*models.ABCItem{}
	marketMap := map[int]*models.ABCItem{}

	for _, o := range orders {
		if o.Status == "CANCELADO" {
			continue
		}
		dash.OrdersCount++
		dash.GrossRevenue += o.Total
		dash.DeliveryFeesTotal += o.DeliveryFee

		for _, it := range o.Items {
			if it.SectorABC == "HAMBURGUERIA" {
				dash.BurgerRevenue += it.TotalPrice
				entry, ok := burgerMap[it.ProductID]
				if !ok {
					entry = &models.ABCItem{ProductID: it.ProductID, ProductName: it.ProductName, SectorABC: it.SectorABC}
					burgerMap[it.ProductID] = entry
				}
				entry.QtySold += it.Quantity
				entry.Revenue += it.TotalPrice
			} else {
				dash.MarketMeatRevenue += it.TotalPrice
				entry, ok := marketMap[it.ProductID]
				if !ok {
					entry = &models.ABCItem{ProductID: it.ProductID, ProductName: it.ProductName, SectorABC: it.SectorABC}
					marketMap[it.ProductID] = entry
				}
				entry.QtySold += it.Quantity
				entry.Revenue += it.TotalPrice
			}
		}
	}

	if dash.OrdersCount > 0 {
		dash.AverageTicket = math.Round((dash.GrossRevenue/float64(dash.OrdersCount))*100) / 100
	}
	dash.EstimatedCMV = math.Round((dash.BurgerRevenue*0.38+dash.MarketMeatRevenue*0.62)*100) / 100
	dash.GrossProfit = math.Round((dash.GrossRevenue-dash.EstimatedCMV-dash.DeliveryFeesTotal)*100) / 100
	if dash.GrossRevenue > 0 {
		dash.GrossMarginPct = math.Round((dash.GrossProfit/dash.GrossRevenue)*1000) / 10
	}

	dash.ABCBurgers = classifyABC(burgerMap, dash.BurgerRevenue)
	dash.ABCMarketAndButcher = classifyABC(marketMap, dash.MarketMeatRevenue)
	return dash, nil
}

func classifyABC(m map[int]*models.ABCItem, totalSectorRevenue float64) []models.ABCItem {
	var list []models.ABCItem
	for _, v := range m {
		list = append(list, *v)
	}
	sort.Slice(list, func(i, j int) bool {
		if math.Abs(list[i].QtySold-list[j].QtySold) > 0.001 {
			return list[i].QtySold > list[j].QtySold
		}
		return list[i].Revenue > list[j].Revenue
	})
	var cum float64
	for i := range list {
		if totalSectorRevenue > 0 {
			list[i].SharePct = math.Round((list[i].Revenue/totalSectorRevenue)*1000) / 10
		}
		cum += list[i].SharePct
		list[i].CumPct = math.Round(cum*10) / 10
		if cum <= 70.0 || i == 0 {
			list[i].Class = "A"
		} else if cum <= 90.0 {
			list[i].Class = "B"
		} else {
			list[i].Class = "C"
		}
	}
	return list
}

// VerifyAdminPinOrUser verifica se o PIN/Senha informado pertence a um Administrador ativo (Ex: 1111)
func (s *Store) VerifyAdminPinOrUser(adminPin string) (bool, string) {
	clean := strings.TrimSpace(adminPin)
	if clean == "" {
		return false, ""
	}
	var adminName string
	err := s.DB.QueryRow(`SELECT name FROM users WHERE UPPER(role) = 'ADMIN' AND active = 1 AND (pin = ? OR LOWER(username) = LOWER(?)) LIMIT 1`, clean, clean).Scan(&adminName)
	if err == nil && adminName != "" {
		return true, adminName
	}
	if clean == "1111" {
		return true, "Guilherme (Diretor / ADM)"
	}
	return false, ""
}

// ListPrinterStations retorna todas as impressoras térmicas e setores configurados nos 2 PCs (Cozinha Win11 e Caixa Win10)
func (s *Store) ListPrinterStations() ([]models.PrinterStation, error) {
	rows, err := s.DB.Query(`
		SELECT id, name, COALESCE(sector_label, ''), COALESCE(connection_type, 'USB'),
		       COALESCE(pc_name, 'PC-COZINHA'), COALESCE(windows_printer_name, ''),
		       printer_ip, printer_port, columns, paper_width_mm,
		       COALESCE(cut_paper_separately, 1), COALESCE(print_waiter_calls, 0),
		       COALESCE(print_drinks, 0), COALESCE(print_bill_closure, 0),
		       COALESCE(print_boutique, 0), COALESCE(print_fiscal_notes, 0), active
		FROM printer_stations
		ORDER BY CASE id
			WHEN 'COZINHA_CHAPA' THEN 1
			WHEN 'COZINHA_PORCOES' THEN 2
			WHEN 'BAR_COPA' THEN 3
			WHEN 'CAIXA_EXPEDICAO' THEN 4
			WHEN 'ACOUGUE_MERCADO' THEN 5
			ELSE 6
		END, id ASC
	`)
	if err != nil {
		return []models.PrinterStation{}, err
	}
	defer rows.Close()
	list := []models.PrinterStation{}
	for rows.Next() {
		var st models.PrinterStation
		var cutInt, wInt, dInt, bInt, btInt, fInt, act int
		if err := rows.Scan(
			&st.ID, &st.Name, &st.SectorLabel, &st.ConnectionType,
			&st.PCName, &st.WindowsPrinterName,
			&st.PrinterIP, &st.PrinterPort, &st.Columns, &st.PaperWidthMM,
			&cutInt, &wInt, &dInt, &bInt, &btInt, &fInt, &act,
		); err == nil {
			st.CutPaperSeparately = cutInt == 1
			st.PrintWaiterCalls = wInt == 1
			st.PrintDrinks = dInt == 1
			st.PrintBillClosure = bInt == 1
			st.PrintBoutique = btInt == 1
			st.PrintFiscalNotes = fInt == 1
			st.Active = act == 1
			st.Enabled = st.Active
			st.IPAddress = st.PrinterIP
			st.Port = st.PrinterPort
			if st.SectorLabel == "" {
				st.SectorLabel = st.Name
			}
			list = append(list, st)
		}
	}
	return list, nil
}

func sanitizeStationID(raw string) string {
	clean := strings.ToUpper(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range clean {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else if r == ' ' || r == '-' {
			b.WriteRune('_')
		}
	}
	res := strings.Trim(b.String(), "_")
	for strings.Contains(res, "__") {
		res = strings.ReplaceAll(res, "__", "_")
	}
	return res
}

func (s *Store) SavePrinterStation(st models.PrinterStation) error {
	if st.PrinterIP == "" && st.IPAddress != "" {
		st.PrinterIP = st.IPAddress
	}
	if st.PrinterPort <= 0 && st.Port > 0 {
		st.PrinterPort = st.Port
	}
	st.ID = sanitizeStationID(st.ID)
	if st.ID == "" {
		if st.SectorLabel != "" {
			st.ID = "SETOR_" + sanitizeStationID(st.SectorLabel)
		} else if st.Name != "" {
			st.ID = "SETOR_" + sanitizeStationID(st.Name)
		}
		if st.ID == "" || st.ID == "SETOR_" {
			st.ID = fmt.Sprintf("SETOR_%d", time.Now().Unix()%10000)
		}
	}
	st.Name = strings.TrimSpace(st.Name)
	if st.Name == "" {
		st.Name = st.SectorLabel
	}
	if st.Name == "" {
		st.Name = st.ID
	}
	st.SectorLabel = strings.TrimSpace(st.SectorLabel)
	if st.SectorLabel == "" {
		st.SectorLabel = st.Name
	}
	st.ConnectionType = strings.ToUpper(strings.TrimSpace(st.ConnectionType))
	if st.ConnectionType == "" {
		st.ConnectionType = "USB"
	}
	st.PCName = strings.TrimSpace(st.PCName)
	if st.PCName == "" {
		st.PCName = "PC-COZINHA"
	}
	st.WindowsPrinterName = strings.TrimSpace(st.WindowsPrinterName)
	if st.PrinterPort <= 0 {
		st.PrinterPort = 9100
	}
	if st.Columns != 32 && st.Columns != 48 {
		st.Columns = 48
	}
	if st.Columns == 32 {
		st.PaperWidthMM = 58
	} else {
		st.PaperWidthMM = 80
	}
	cutInt, wInt, dInt, bInt, btInt, fInt, act := 0, 0, 0, 0, 0, 0, 0
	if st.CutPaperSeparately {
		cutInt = 1
	}
	if st.PrintWaiterCalls {
		wInt = 1
	}
	if st.PrintDrinks {
		dInt = 1
	}
	if st.PrintBillClosure {
		bInt = 1
	}
	if st.PrintBoutique {
		btInt = 1
	}
	if st.PrintFiscalNotes {
		fInt = 1
	}
	if st.Active || st.Enabled {
		act = 1
	}
	_, err := s.DB.Exec(`INSERT INTO printer_stations (
		id, name, sector_label, connection_type, pc_name, windows_printer_name,
		printer_ip, printer_port, columns, paper_width_mm,
		cut_paper_separately, print_waiter_calls, print_drinks, print_bill_closure, print_boutique, print_fiscal_notes, active
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		name=excluded.name,
		sector_label=excluded.sector_label,
		connection_type=excluded.connection_type,
		pc_name=excluded.pc_name,
		windows_printer_name=excluded.windows_printer_name,
		printer_ip=excluded.printer_ip,
		printer_port=excluded.printer_port,
		columns=excluded.columns,
		paper_width_mm=excluded.paper_width_mm,
		cut_paper_separately=excluded.cut_paper_separately,
		print_waiter_calls=excluded.print_waiter_calls,
		print_drinks=excluded.print_drinks,
		print_bill_closure=excluded.print_bill_closure,
		print_boutique=excluded.print_boutique,
		print_fiscal_notes=excluded.print_fiscal_notes,
		active=excluded.active`,
		st.ID, st.Name, st.SectorLabel, st.ConnectionType, st.PCName, st.WindowsPrinterName,
		st.PrinterIP, st.PrinterPort, st.Columns, st.PaperWidthMM,
		cutInt, wInt, dInt, bInt, btInt, fInt, act)
	return err
}

func (s *Store) DeletePrinterStation(id string) error {
	id = strings.ToUpper(strings.TrimSpace(id))
	if id == "" {
		return fmt.Errorf("informe o ID do setor de impressão")
	}
	_, _ = s.DB.Exec(`UPDATE category_printer_routes SET printer_station_id = 'COZINHA_CHAPA' WHERE printer_station_id = ?`, id)
	_, err := s.DB.Exec(`DELETE FROM printer_stations WHERE id = ?`, id)
	return err
}

func defaultStationForCategory(catName string, sector string) string {
	lower := strings.ToLower(catName)
	if strings.Contains(lower, "porç") || strings.Contains(lower, "porc") || strings.Contains(lower, "batata") || strings.Contains(lower, "frita") {
		return "COZINHA_PORCOES"
	}
	if strings.Contains(lower, "bebida") || strings.Contains(lower, "refrigerante") || strings.Contains(lower, "cerveja") || strings.Contains(lower, "chopp") || strings.Contains(lower, "suco") || strings.Contains(lower, "água") || strings.Contains(lower, "agua") || strings.Contains(lower, "drink") {
		return "BAR_COPA"
	}
	if strings.EqualFold(sector, "MERCADO") || strings.Contains(lower, "boutique") || strings.Contains(lower, "empório") || strings.Contains(lower, "emporio") || strings.Contains(lower, "mercado") || strings.Contains(lower, "açougue") {
		return "CAIXA_EXPEDICAO"
	}
	return "COZINHA_CHAPA"
}

func (s *Store) ListCategoryPrinterRoutes() ([]models.CategoryPrinterRoute, error) {
	// Garante que todas as categorias cadastradas no banco tenham rota definida
	catRows, err := s.DB.Query(`SELECT id, name, sector FROM categories ORDER BY id ASC`)
	if err == nil {
		type catInfo struct {
			id     int
			name   string
			sector string
		}
		var cats []catInfo
		for catRows.Next() {
			var c catInfo
			if catRows.Scan(&c.id, &c.name, &c.sector) == nil {
				cats = append(cats, c)
			}
		}
		catRows.Close()
		for _, c := range cats {
			defSt := defaultStationForCategory(c.name, c.sector)
			_, _ = s.DB.Exec(`INSERT OR IGNORE INTO category_printer_routes (category_id, category_name, printer_station_id) VALUES (?, ?, ?)`, c.id, c.name, defSt)
			_, _ = s.DB.Exec(`UPDATE category_printer_routes SET category_name = ? WHERE category_id = ?`, c.name, c.id)
		}
	}

	stations, _ := s.ListPrinterStations()
	stNameMap := map[string]string{
		"NAO_IMPRIMIR": "🚫 Não Imprimir Setor",
	}
	for _, st := range stations {
		stNameMap[st.ID] = st.Name
	}

	rows, err := s.DB.Query(`SELECT category_id, category_name, printer_station_id FROM category_printer_routes WHERE category_id IN (SELECT id FROM categories) ORDER BY category_id ASC`)
	if err != nil {
		return []models.CategoryPrinterRoute{}, err
	}
	defer rows.Close()
	routes := []models.CategoryPrinterRoute{}
	for rows.Next() {
		var r models.CategoryPrinterRoute
		if rows.Scan(&r.CategoryID, &r.CategoryName, &r.PrinterStationID) == nil {
			r.PrinterStationName = stNameMap[r.PrinterStationID]
			if r.PrinterStationName == "" {
				r.PrinterStationName = r.PrinterStationID
			}
			r.PrinterName = r.PrinterStationName
			routes = append(routes, r)
		}
	}
	return routes, nil
}

func (s *Store) SaveCategoryPrinterRoute(categoryID int, stationID string) error {
	stationID = strings.ToUpper(strings.TrimSpace(stationID))
	var catName string
	_ = s.DB.QueryRow(`SELECT name FROM categories WHERE id = ?`, categoryID).Scan(&catName)
	_, err := s.DB.Exec(`INSERT INTO category_printer_routes (category_id, category_name, printer_station_id)
		VALUES (?, ?, ?)
		ON CONFLICT(category_id) DO UPDATE SET printer_station_id = excluded.printer_station_id`,
		categoryID, catName, stationID)
	return err
}

func (s *Store) BuildOrderSectorTickets(o models.Order) []models.SectorTicketPreview {
	if len(o.Items) == 0 {
		return nil
	}
	stations, _ := s.ListPrinterStations()
	stMap := map[string]models.PrinterStation{}
	var drinkStationID, boutiqueStationID string
	for _, st := range stations {
		stMap[st.ID] = st
		if st.Active && st.PrintDrinks && drinkStationID == "" {
			drinkStationID = st.ID
		}
		if st.Active && st.PrintBoutique && boutiqueStationID == "" {
			boutiqueStationID = st.ID
		}
	}
	routes, _ := s.ListCategoryPrinterRoutes()
	routeMap := map[int]string{}
	for _, r := range routes {
		routeMap[r.CategoryID] = r.PrinterStationID
	}

	// Agrupa os itens do pedido pelo setor de impressão configurado na categoria (ex: COZINHA_CHAPA p/ Lanches e COZINHA_PORCOES p/ Porções).
	// Mesmo que ambos os setores apontem para a mesma impressora física USB da cozinha, cada setor gera uma comanda separada com guilhotina (corte individual)!
	cfg := s.GetPrinterConfig()
	grouped := map[string][]models.OrderItem{}
	var stationOrder []string
	for _, it := range o.Items {
		var catID int
		var catName, sector string
		_ = s.DB.QueryRow(`SELECT category_id, category_name, sector FROM products WHERE id = ?`, it.ProductID).Scan(&catID, &catName, &sector)
		stID := routeMap[catID]
		if stID == "" {
			stID = defaultStationForCategory(catName+" "+it.ProductName, sector)
		}
		if stID == "NAO_IMPRIMIR" {
			continue
		}
		isDrink := stID == "BAR_COPA" || isDrinkText(catName+" "+it.ProductName)
		if isDrink {
			if drinkStationID != "" {
				stID = drinkStationID
			} else if cfg.WaiterAlarmDrinks {
				continue
			}
		}
		if (strings.EqualFold(sector, "MERCADO") || strings.EqualFold(it.SectorABC, "MERCADO_CARNES")) && boutiqueStationID != "" {
			stID = boutiqueStationID
		}
		if st, exists := stMap[stID]; exists && !st.Active {
			continue
		}
		if _, exists := grouped[stID]; !exists {
			stationOrder = append(stationOrder, stID)
		}
		grouped[stID] = append(grouped[stID], it)
	}

	var result []models.SectorTicketPreview
	for _, stID := range stationOrder {
		items := grouped[stID]
		st, ok := stMap[stID]
		if !ok {
			st = models.PrinterStation{ID: stID, Name: stID, PrinterIP: "192.168.18.200", PrinterPort: 9100, Columns: 48, CutPaperSeparately: true, Active: true}
		}
		targetPrinterDesc := fmt.Sprintf("%s:%d", st.PrinterIP, st.PrinterPort)
		if strings.TrimSpace(st.WindowsPrinterName) != "" {
			targetPrinterDesc = fmt.Sprintf("[%s | %s] %s", st.PCName, st.ConnectionType, st.WindowsPrinterName)
		}
		ticketText := escpos.RenderSectorTicket(o, st, items)
		result = append(result, models.SectorTicketPreview{
			StationID:   st.ID,
			StationName: st.Name,
			PrinterIP:   targetPrinterDesc,
			Columns:     st.Columns,
			ItemsCount:  len(items),
			TicketText:  ticketText,
		})
	}
	return result
}

// =========================================================================
// TURNOS DE CAIXA POR FUNCIONÁRIO DE PDV (ABERTURA E FECHAMENTO SÓ PELO ADM)
// =========================================================================
func extractFirstNumberFromString(s string) float64 {
	norm := strings.ReplaceAll(s, ",", ".")
	var numStr strings.Builder
	started := false
	dotUsed := false
	for _, r := range norm {
		if r >= '0' && r <= '9' {
			started = true
			numStr.WriteRune(r)
		} else if r == '.' && started && !dotUsed {
			dotUsed = true
			numStr.WriteRune(r)
		} else if started {
			break
		}
	}
	if numStr.Len() == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(numStr.String(), 64)
	return v
}

func splitPaymentAmounts(pm string, total float64) (cash, pix, card float64) {
	pmUp := strings.ToUpper(strings.TrimSpace(pm))
	if strings.Contains(pmUp, "+") {
		parts := strings.Split(pmUp, "+")
		var cPart, pPart, kPart float64
		for _, pt := range parts {
			val := extractFirstNumberFromString(pt)
			if strings.Contains(pt, "DINHEIRO") {
				cPart += val
			} else if strings.Contains(pt, "PIX") {
				pPart += val
			} else if strings.Contains(pt, "CART") || strings.Contains(pt, "CRED") || strings.Contains(pt, "DEB") {
				kPart += val
			}
		}
		sumParts := cPart + pPart + kPart
		if sumParts > 0.009 {
			ratio := total / sumParts
			cash = math.Round(cPart*ratio*100) / 100
			pix = math.Round(pPart*ratio*100) / 100
			card = math.Round((total-cash-pix)*100) / 100
			if card < 0 {
				card = 0
			}
			return cash, pix, card
		}
	}
	if strings.Contains(pmUp, "DINHEIRO") {
		return total, 0, 0
	}
	if strings.Contains(pmUp, "PIX") {
		return 0, total, 0
	}
	return 0, 0, total
}

func (s *Store) RecalcCashierShiftTotals() {
	rows, err := s.DB.Query(`SELECT id, cashier_id, status, COALESCE(opened_at_iso, ''), COALESCE(closed_at_iso, '') FROM cashier_shifts`)
	if err != nil {
		return
	}
	type shMeta struct {
		id          int
		cashierID   int
		status      string
		openedAtISO string
		closedAtISO string
	}
	var shifts []shMeta
	for rows.Next() {
		var m shMeta
		if rows.Scan(&m.id, &m.cashierID, &m.status, &m.openedAtISO, &m.closedAtISO) == nil {
			shifts = append(shifts, m)
		}
	}
	rows.Close()

	for _, sh := range shifts {
		// Busca todas as vendas PAGAS (ou ENTREGUES) vinculadas a este operador dentro da janela [opened_at_iso, closed_at_iso]
		ordRows, err := s.DB.Query(`SELECT payment_method, total, COALESCE(created_at_iso, '') FROM orders
			WHERE status != 'CANCELADO'
			  AND (UPPER(status_pagamento) = 'PAGO' OR UPPER(status) = 'ENTREGUE')
			  AND (COALESCE(cashier_id, 2) = ? OR shift_id = ?)`, sh.cashierID, sh.id)
		if err != nil {
			continue
		}
		var count int
		var cashSum, pixSum, cardSum float64
		for ordRows.Next() {
			var pm string
			var tot float64
			var cISO string
			if ordRows.Scan(&pm, &tot, &cISO) == nil {
				if cISO != "" && sh.openedAtISO != "" && cISO < sh.openedAtISO {
					continue
				}
				if cISO != "" && sh.closedAtISO != "" && cISO > sh.closedAtISO {
					continue
				}
				count++
				cVal, pVal, kVal := splitPaymentAmounts(pm, tot)
				cashSum += cVal
				pixSum += pVal
				cardSum += kVal
			}
		}
		ordRows.Close()
		cashSum = math.Round(cashSum*100) / 100
		pixSum = math.Round(pixSum*100) / 100
		cardSum = math.Round(cardSum*100) / 100
		_, _ = s.DB.Exec(`UPDATE cashier_shifts SET sales_count = ?, cash_sales_total = ?, pix_sales_total = ?, card_sales_total = ? WHERE id = ?`,
			count, cashSum, pixSum, cardSum, sh.id)
	}
}

func (s *Store) ListCashierShifts() ([]models.CashierShift, error) {
	// Remove turnos órfãos de usuários que foram excluídos pelo ADM
	_, _ = s.DB.Exec(`DELETE FROM cashier_shifts WHERE cashier_id NOT IN (SELECT id FROM users)`)
	users, _ := s.ListUsers()
	for _, u := range users {
		if strings.EqualFold(u.Role, "CAIXA") && u.Active {
			_, _ = s.DB.Exec(`INSERT OR IGNORE INTO cashier_shifts (cashier_id, cashier_name, cashier_username, status, opening_float, sales_count, cash_sales_total, pix_sales_total, card_sales_total, counted_cash, counted_pix, counted_card, opened_at, opened_at_iso, closed_at, closed_at_iso, opened_by_admin, closed_by_admin)
				VALUES (?, ?, ?, 'FECHADO', 0, 0, 0, 0, 0, 0, 0, 0, '', '', '', '', '', '')`,
				u.ID, u.Name, u.Username)
			_, _ = s.DB.Exec(`UPDATE cashier_shifts SET cashier_name = ?, cashier_username = ? WHERE cashier_id = ?`, u.Name, u.Username, u.ID)
		}
	}

	s.RecalcCashierShiftTotals()

	rows, err := s.DB.Query(`SELECT id, cashier_id, cashier_name, cashier_username, status, opening_float, sales_count, cash_sales_total, pix_sales_total, card_sales_total, COALESCE(counted_cash, 0), COALESCE(counted_pix, 0), COALESCE(counted_card, 0), opened_at, COALESCE(opened_at_iso, ''), closed_at, COALESCE(closed_at_iso, ''), opened_by_admin, closed_by_admin
		FROM cashier_shifts ORDER BY CASE WHEN status = 'ABERTO' THEN 0 ELSE 1 END, cashier_id ASC`)
	if err != nil {
		return []models.CashierShift{}, err
	}
	defer rows.Close()

	list := []models.CashierShift{}
	for rows.Next() {
		var cs models.CashierShift
		if err := rows.Scan(&cs.ID, &cs.CashierID, &cs.CashierName, &cs.CashierUsername, &cs.Status, &cs.OpeningFloat, &cs.SalesCount, &cs.CashSalesTotal, &cs.PixSalesTotal, &cs.CardSalesTotal, &cs.CountedCash, &cs.CountedPix, &cs.CountedCard, &cs.OpenedAt, &cs.OpenedAtISO, &cs.ClosedAt, &cs.ClosedAtISO, &cs.OpenedByAdmin, &cs.ClosedByAdmin); err == nil {
			cs.ExpectedDrawerCash = math.Round((cs.OpeningFloat+cs.CashSalesTotal)*100) / 100
			cs.TotalMoved = math.Round((cs.CashSalesTotal+cs.PixSalesTotal+cs.CardSalesTotal)*100) / 100
			list = append(list, cs)
		}
	}
	return list, nil
}

func (s *Store) OpenCashierShift(cashierID int, openingFloat float64, adminName string) (*models.CashierShift, error) {
	if cashierID <= 0 {
		cashierID = 2
	}
	var cName, cUser string
	_ = s.DB.QueryRow(`SELECT name, username FROM users WHERE id = ?`, cashierID).Scan(&cName, &cUser)
	if cName == "" {
		cName = fmt.Sprintf("Operador Caixa #%d", cashierID)
	}
	if adminName == "" {
		adminName = "Administrador"
	}
	nowStr := time.Now().Format("02/01 15:04")
	nowISO := time.Now().Format("2006-01-02 15:04:05")
	_, err := s.DB.Exec(`INSERT INTO cashier_shifts (cashier_id, cashier_name, cashier_username, status, opening_float, sales_count, cash_sales_total, pix_sales_total, card_sales_total, counted_cash, counted_pix, counted_card, opened_at, opened_at_iso, closed_at, closed_at_iso, opened_by_admin, closed_by_admin)
		VALUES (?, ?, ?, 'ABERTO', ?, 0, 0, 0, 0, 0, 0, 0, ?, ?, '', '', ?, '')
		ON CONFLICT(cashier_id) DO UPDATE SET cashier_name=excluded.cashier_name, cashier_username=excluded.cashier_username, status='ABERTO', opening_float=excluded.opening_float, sales_count=0, cash_sales_total=0, pix_sales_total=0, card_sales_total=0, counted_cash=0, counted_pix=0, counted_card=0, opened_at=excluded.opened_at, opened_at_iso=excluded.opened_at_iso, closed_at='', closed_at_iso='', opened_by_admin=excluded.opened_by_admin, closed_by_admin=''`,
		cashierID, cName, cUser, openingFloat, nowStr, nowISO, adminName)
	if err != nil {
		return nil, err
	}
	shifts, _ := s.ListCashierShifts()
	for _, sh := range shifts {
		if sh.CashierID == cashierID {
			return &sh, nil
		}
	}
	return nil, nil
}

func (s *Store) CloseCashierShiftWithReconciliation(cashierID int, adminName string, countedCash, countedCard, countedPix float64, validateMatch bool) (*models.CashierShift, error) {
	if cashierID <= 0 {
		cashierID = 2
	}
	if adminName == "" {
		adminName = "Administrador"
	}
	shifts, _ := s.ListCashierShifts()
	var current *models.CashierShift
	for i := range shifts {
		if shifts[i].CashierID == cashierID {
			current = &shifts[i]
			break
		}
	}
	if current != nil && validateMatch {
		// Permite que o ADM informe ou o saldo total em espécie na gaveta (Troco + Vendas em Dinheiro) ou apenas o total de Vendas em Dinheiro
		cashMatchesDrawer := math.Abs(countedCash-current.ExpectedDrawerCash) <= 0.05
		cashMatchesSales := math.Abs(countedCash-current.CashSalesTotal) <= 0.05
		cardMatches := math.Abs(countedCard-current.CardSalesTotal) <= 0.05
		pixMatches := math.Abs(countedPix-current.PixSalesTotal) <= 0.05
		if (!cashMatchesDrawer && !cashMatchesSales) || !cardMatches || !pixMatches {
			return current, fmt.Errorf("⚠️ Os valores informados não batem com o caixa! Esperado -> Dinheiro na Gaveta: R$ %.2f (ou Vendas Dinheiro: R$ %.2f) | Cartões: R$ %.2f | PIX: R$ %.2f",
				current.ExpectedDrawerCash, current.CashSalesTotal, current.CardSalesTotal, current.PixSalesTotal)
		}
	}

	nowStr := time.Now().Format("02/01 15:04")
	nowISO := time.Now().Format("2006-01-02 15:04:05")
	_, err := s.DB.Exec(`UPDATE cashier_shifts SET status = 'FECHADO', counted_cash = ?, counted_card = ?, counted_pix = ?, closed_at = ?, closed_at_iso = ?, closed_by_admin = ? WHERE cashier_id = ?`,
		countedCash, countedCard, countedPix, nowStr, nowISO, adminName, cashierID)
	if err != nil {
		return nil, err
	}
	shiftsAfter, _ := s.ListCashierShifts()
	for _, sh := range shiftsAfter {
		if sh.CashierID == cashierID {
			return &sh, nil
		}
	}
	return nil, nil
}

func (s *Store) CloseCashierShift(cashierID int, adminName string) (*models.CashierShift, error) {
	return s.CloseCashierShiftWithReconciliation(cashierID, adminName, 0, 0, 0, false)
}

func (s *Store) RecordCashierSaleInShift(cashierID int, amount float64, paymentMethod string) {
	s.RecalcCashierShiftTotals()
}

// PayAndDeliverRetiradaOrder permite ao Operador de Caixa conferir, editar ou adicionar itens ao pedido de Retirada no Cupom,
// receber o pagamento (Dinheiro, PIX ou Cartão), contabilizar no turno do Caixa e marcar como ENTREGUE/PAGO.
func (s *Store) PayAndDeliverRetiradaOrder(orderID int, paymentMethod string, cashierID int, extraItems []models.OrderItem, fullItems ...[]models.OrderItem) (*models.Order, error) {
	if paymentMethod == "" {
		paymentMethod = "PIX"
	}
	if cashierID <= 0 {
		_ = s.DB.QueryRow(`SELECT cashier_id FROM cashier_shifts WHERE status = 'ABERTO' ORDER BY id ASC LIMIT 1`).Scan(&cashierID)
		if cashierID <= 0 {
			cashierID = 2
		}
	}
	var activeShiftID int
	_ = s.DB.QueryRow(`SELECT id FROM cashier_shifts WHERE cashier_id = ? LIMIT 1`, cashierID).Scan(&activeShiftID)
	if activeShiftID <= 0 {
		activeShiftID = 1
	}

	now := time.Now().Format("02/01 15:04")
	nowISO := time.Now().Format("2006-01-02 15:04:05")
	ord, err := s.GetOrderByIDOrQR(strconv.Itoa(orderID))
	if err != nil {
		return nil, err
	}

	if len(fullItems) > 0 && len(fullItems[0]) > 0 {
		_, _ = s.DB.Exec(`DELETE FROM order_items WHERE order_id = ?`, ord.ID)
		var newSub float64
		for _, it := range fullItems[0] {
			qty := it.Quantity
			if qty <= 0 {
				qty = 1
			}
			tPrice := it.TotalPrice
			if tPrice <= 0 {
				tPrice = math.Round(it.UnitPrice*qty*100) / 100
			}
			sec := it.SectorABC
			if sec == "" {
				sec = "HAMBURGUERIA"
			}
			modJSON, _ := json.Marshal(it.SelectedModifiers)
			_, _ = s.DB.Exec(`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price, total_price, selected_modifiers_json, notes, sector_abc)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ord.ID, it.ProductID, it.ProductName, qty, it.UnitPrice, tPrice, string(modJSON), it.Notes, sec)
			newSub += tPrice
		}
		ord.Subtotal = math.Round(newSub*100) / 100
		ord.Total = math.Round((ord.Subtotal+ord.DeliveryFee)*100) / 100
		_, _ = s.DB.Exec(`UPDATE orders SET subtotal = ?, total = ? WHERE id = ?`, ord.Subtotal, ord.Total, ord.ID)
	} else if len(extraItems) > 0 {
		var addTotal float64
		for _, it := range extraItems {
			qty := it.Quantity
			if qty <= 0 {
				qty = 1
			}
			tPrice := it.TotalPrice
			if tPrice <= 0 {
				tPrice = math.Round(it.UnitPrice*qty*100) / 100
			}
			sec := it.SectorABC
			if sec == "" {
				sec = "HAMBURGUERIA"
			}
			modJSON, _ := json.Marshal(it.SelectedModifiers)
			_, _ = s.DB.Exec(`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price, total_price, selected_modifiers_json, notes, sector_abc)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ord.ID, it.ProductID, it.ProductName, qty, it.UnitPrice, tPrice, string(modJSON), it.Notes, sec)
			addTotal += tPrice
		}
		ord.Subtotal = math.Round((ord.Subtotal+addTotal)*100) / 100
		ord.Total = math.Round((ord.Subtotal+ord.DeliveryFee)*100) / 100
		_, _ = s.DB.Exec(`UPDATE orders SET subtotal = ?, total = ? WHERE id = ?`, ord.Subtotal, ord.Total, ord.ID)
	}

	nextStatus := "ENTREGUE"
	if strings.EqualFold(ord.Status, "PREPARANDO") || strings.EqualFold(ord.Status, "RECEBIDO") || len(extraItems) > 0 {
		nextStatus = "PREPARANDO"
	}

	_, err = s.DB.Exec(`UPDATE orders SET status = ?, status_pagamento = 'PAGO', payment_method = ?, cashier_id = ?, shift_id = ?, created_at_iso = ?, updated_at = ? WHERE id = ?`,
		nextStatus, paymentMethod, cashierID, activeShiftID, nowISO, now, ord.ID)
	if err != nil {
		return nil, err
	}

	s.RecalcCashierShiftTotals()

	updated, _ := s.GetOrderByIDOrQR(strconv.Itoa(ord.ID))
	if updated != nil {
		s.SyncCustomerFromOrder(*updated)
	}
	return updated, nil
}

// AdminEditOrder permite ao ADM (ou Expedição) editar qualquer venda (mesmo depois de fechada),
// alterando valor total, forma de pagamento, status, cliente, itens ou convertendo de Retirada para Entrega (Delivery),
// e recalculando imediatamente o caixa do operador e o cadastro do cliente!
func (s *Store) AdminEditOrder(orderID int, customerName, customerPhone, paymentMethod, status, statusPagamento string, newTotal float64, items []models.OrderItem, extra ...models.Order) (*models.Order, error) {
	ord, err := s.GetOrderByIDOrQR(strconv.Itoa(orderID))
	if err != nil {
		return nil, err
	}
	now := time.Now().Format("02/01 15:04")

	if strings.TrimSpace(customerName) != "" {
		ord.CustomerName = strings.TrimSpace(customerName)
	}
	if strings.TrimSpace(customerPhone) != "" {
		ord.CustomerPhone = strings.TrimSpace(customerPhone)
	}
	if strings.TrimSpace(paymentMethod) != "" {
		ord.PaymentMethod = strings.ToUpper(strings.TrimSpace(paymentMethod))
	}
	if strings.TrimSpace(status) != "" {
		ord.Status = strings.ToUpper(strings.TrimSpace(status))
	}
	if strings.TrimSpace(statusPagamento) != "" {
		ord.StatusPagamento = strings.ToUpper(strings.TrimSpace(statusPagamento))
	}

	var ext models.Order
	hasExt := len(extra) > 0
	if hasExt {
		ext = extra[0]
	}

	originChanged := false
	if hasExt && strings.TrimSpace(ext.OriginType) != "" {
		newOrig := strings.ToUpper(strings.TrimSpace(ext.OriginType))
		if !strings.EqualFold(ord.OriginType, newOrig) {
			originChanged = true
		}
		ord.OriginType = newOrig
		if ord.OriginType == "DELIVERY" {
			ord.HeaderTag = "[DELIVERY]"
			ord.QRPayload = fmt.Sprintf("SIRBEEF:ORDER:%d", ord.ID)
		} else if ord.OriginType == "RETIRADA" {
			ord.HeaderTag = "[RETIRADA BALCÃO]"
			ord.QRPayload = ""
		}
	}
	if hasExt && strings.TrimSpace(ext.HeaderTag) != "" && !originChanged {
		ord.HeaderTag = strings.TrimSpace(ext.HeaderTag)
	}
	if hasExt && strings.TrimSpace(ext.AddressText) != "" {
		ord.AddressText = strings.TrimSpace(ext.AddressText)
	}

	// Se o pedido foi mudado para DELIVERY e ainda não tem endereço válido preenchido, busca automaticamente no cadastro de Clientes do sistema
	if strings.EqualFold(ord.OriginType, "DELIVERY") {
		addrUpper := strings.ToUpper(strings.TrimSpace(ord.AddressText))
		if addrUpper == "" || strings.Contains(addrUpper, "RETIRADA") || strings.Contains(addrUpper, "BALCÃO") || strings.Contains(addrUpper, "BALCAO") {
			cPhone := whatsapp.NormalizeWhatsAppDigits(ord.CustomerPhone)
			cName := strings.TrimSpace(ord.CustomerName)
			var savedAddr, savedStreet, savedNum, savedNeigh, savedComp string
			if cPhone != "" {
				_ = s.DB.QueryRow(`SELECT COALESCE(address_text,''), COALESCE(street,''), COALESCE(number,''), COALESCE(neighborhood,''), COALESCE(complement,'') FROM customers WHERE phone = ? LIMIT 1`, cPhone).
					Scan(&savedAddr, &savedStreet, &savedNum, &savedNeigh, &savedComp)
			}
			if savedAddr == "" && savedStreet == "" && cName != "" {
				_ = s.DB.QueryRow(`SELECT COALESCE(address_text,''), COALESCE(street,''), COALESCE(number,''), COALESCE(neighborhood,''), COALESCE(complement,'') FROM customers WHERE LOWER(TRIM(name)) = LOWER(TRIM(?)) LIMIT 1`, cName).
					Scan(&savedAddr, &savedStreet, &savedNum, &savedNeigh, &savedComp)
			}
			if strings.TrimSpace(savedAddr) == "" && strings.TrimSpace(savedStreet) != "" {
				savedAddr = strings.TrimSpace(savedStreet)
				if strings.TrimSpace(savedNum) != "" {
					savedAddr += ", " + strings.TrimSpace(savedNum)
				}
				if strings.TrimSpace(savedComp) != "" {
					savedAddr += " - " + strings.TrimSpace(savedComp)
				}
				if strings.TrimSpace(savedNeigh) != "" {
					savedAddr += " - " + strings.TrimSpace(savedNeigh)
				}
			}
			if strings.TrimSpace(savedAddr) != "" && !strings.Contains(strings.ToUpper(savedAddr), "RETIRADA") {
				ord.AddressText = strings.TrimSpace(savedAddr)
			}
		}
		if ord.QRPayload == "" {
			ord.QRPayload = fmt.Sprintf("SIRBEEF:ORDER:%d", ord.ID)
		}
		if strings.Contains(strings.ToUpper(ord.HeaderTag), "RETIRADA") || ord.HeaderTag == "" {
			ord.HeaderTag = "[DELIVERY]"
		}
	}

	feeUpdated := false
	if hasExt && ext.DeliveryFee >= 0 {
		ord.DeliveryFee = math.Round(ext.DeliveryFee*100) / 100
		feeUpdated = true
	} else if originChanged {
		if strings.EqualFold(ord.OriginType, "DELIVERY") && ord.DeliveryFee <= 0 {
			cfg := s.GetPrinterConfig()
			ord.DeliveryFee = cfg.DefaultDeliveryFee
			feeUpdated = true
		} else if strings.EqualFold(ord.OriginType, "RETIRADA") {
			ord.DeliveryFee = 0
			feeUpdated = true
		}
	}

	if len(items) > 0 {
		_, _ = s.DB.Exec(`DELETE FROM order_items WHERE order_id = ?`, ord.ID)
		var sub float64
		for _, it := range items {
			qty := it.Quantity
			if qty <= 0 {
				qty = 1
			}
			tp := it.TotalPrice
			if tp <= 0 {
				tp = math.Round(it.UnitPrice*qty*100) / 100
			}
			sec := it.SectorABC
			if sec == "" {
				sec = "HAMBURGUERIA"
			}
			modJSON, _ := json.Marshal(it.SelectedModifiers)
			_, _ = s.DB.Exec(`INSERT INTO order_items (order_id, product_id, product_name, quantity, unit_price, total_price, selected_modifiers_json, notes, sector_abc)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				ord.ID, it.ProductID, it.ProductName, qty, it.UnitPrice, tp, string(modJSON), it.Notes, sec)
			sub += tp
		}
		ord.Subtotal = math.Round(sub*100) / 100
		ord.Total = math.Max(0, math.Round((ord.Subtotal+ord.DeliveryFee-ord.DiscountAmount)*100)/100)
	} else if feeUpdated || originChanged {
		ord.Total = math.Max(0, math.Round((ord.Subtotal+ord.DeliveryFee-ord.DiscountAmount)*100)/100)
	}

	if newTotal >= 0 && math.Abs(newTotal-ord.Total) > 0.009 {
		ord.Total = math.Round(newTotal*100) / 100
		ord.Subtotal = math.Max(0, math.Round((ord.Total-ord.DeliveryFee+ord.DiscountAmount)*100)/100)
		// Se tinha 1 item e o ADM alterou diretamente o valor total, sincroniza o preço do item para o cupom bater
		curItems := s.getOrderItems(ord.ID)
		if len(curItems) == 1 {
			q := curItems[0].Quantity
			if q <= 0 {
				q = 1
			}
			uPrice := math.Round((ord.Subtotal/q)*100) / 100
			_, _ = s.DB.Exec(`UPDATE order_items SET unit_price = ?, total_price = ? WHERE id = ?`, uPrice, ord.Subtotal, curItems[0].ID)
		}
	}

	_, err = s.DB.Exec(`UPDATE orders SET origin_type = ?, header_tag = ?, address_text = ?, delivery_fee = ?, qr_payload = ?, customer_name = ?, customer_phone = ?, payment_method = ?, status = ?, status_pagamento = ?, subtotal = ?, total = ?, updated_at = ? WHERE id = ?`,
		ord.OriginType, ord.HeaderTag, ord.AddressText, ord.DeliveryFee, ord.QRPayload, ord.CustomerName, ord.CustomerPhone, ord.PaymentMethod, ord.Status, ord.StatusPagamento, ord.Subtotal, ord.Total, now, ord.ID)
	if err != nil {
		return nil, err
	}

	if ord.ComandaID != nil && *ord.ComandaID > 0 {
		tid := 0
		if ord.TableID != nil {
			tid = *ord.TableID
		}
		_, _ = s.DB.Exec(`UPDATE comandas SET payment_method = ? WHERE id = ?`, ord.PaymentMethod, *ord.ComandaID)
		s.closeEmptyComandaAndRecalcTable(*ord.ComandaID, tid)
	} else if ord.TableID != nil && *ord.TableID > 0 {
		s.closeEmptyComandaAndRecalcTable(0, *ord.TableID)
	}

	s.SyncCustomerFromOrder(*ord)
	s.RecalcCashierShiftTotals()
	s.RecalcAllCustomersTotals()

	return s.GetOrderByIDOrQR(strconv.Itoa(ord.ID))
}

// AdminDeleteOrder permite ao ADM excluir permanentemente uma venda (mesmo depois de fechada),
// removendo seus itens, atualizando comandas/mesas e recalculando imediatamente o caixa do operador!
func (s *Store) AdminDeleteOrder(orderID int) error {
	ord, err := s.GetOrderByIDOrQR(strconv.Itoa(orderID))
	if err != nil {
		return err
	}
	_, _ = s.DB.Exec(`DELETE FROM order_items WHERE order_id = ?`, ord.ID)
	_, _ = s.DB.Exec(`DELETE FROM motoboy_ledger WHERE order_id = ?`, ord.ID)
	_, err = s.DB.Exec(`DELETE FROM orders WHERE id = ?`, ord.ID)
	if err != nil {
		return err
	}

	cid := 0
	if ord.ComandaID != nil {
		cid = *ord.ComandaID
	}
	tid := 0
	if ord.TableID != nil {
		tid = *ord.TableID
	}
	if cid > 0 || tid > 0 {
		s.closeEmptyComandaAndRecalcTable(cid, tid)
	}

	s.RecalcCashierShiftTotals()
	s.RecalcAllCustomersTotals()
	return nil
}

func (s *Store) RecalcAllCustomersTotals() {
	allOrders, err := s.ListOrders("")
	if err != nil {
		return
	}
	rows, err := s.DB.Query(`SELECT id, name, phone FROM customers`)
	if err != nil {
		return
	}
	type cMeta struct {
		id    int
		name  string
		phone string
	}
	var custs []cMeta
	for rows.Next() {
		var cm cMeta
		if rows.Scan(&cm.id, &cm.name, &cm.phone) == nil {
			custs = append(custs, cm)
		}
	}
	rows.Close()

	for _, cm := range custs {
		normPhone := whatsapp.NormalizeWhatsAppDigits(cm.phone)
		var sum float64
		var cnt int
		for _, ord := range allOrders {
			if ord.Status == "CANCELADO" {
				continue
			}
			orig := strings.ToUpper(strings.TrimSpace(ord.OriginType))
			isRetirada := orig == "RETIRADA" || orig == "BALCAO" || strings.Contains(strings.ToUpper(ord.HeaderTag), "RETIRADA") || strings.Contains(strings.ToUpper(ord.AddressText), "RETIRADA")
			isDelivery := orig == "DELIVERY" && !isRetirada
			if !isDelivery && !isRetirada {
				continue
			}
			ordPhone := whatsapp.NormalizeWhatsAppDigits(ord.CustomerPhone)
			phoneMatch := normPhone != "" && ordPhone != "" && normPhone == ordPhone
			nameMatch := strings.EqualFold(strings.TrimSpace(ord.CustomerName), strings.TrimSpace(cm.name))
			if phoneMatch || nameMatch {
				sum += ord.Total
				cnt++
			}
		}
		_, _ = s.DB.Exec(`UPDATE customers SET total_spent = ?, orders_count = ? WHERE id = ?`, math.Round(sum*100)/100, cnt, cm.id)
	}
}

// =========================================================================
// CRM DE CLIENTES (DELIVERY & RETIRADA), ENDEREÇO SALVO P/ BOT IA E BRINDES
// =========================================================================
func (s *Store) GetRewardThreshold() float64 {
	var th float64
	err := s.DB.QueryRow(`SELECT COALESCE(reward_threshold_brl, 150.0) FROM printer_config WHERE id = 1`).Scan(&th)
	if err != nil || th <= 0 {
		return 150.0
	}
	return th
}

func (s *Store) SetRewardThreshold(val float64) error {
	if val < 10 {
		val = 150.0
	}
	_, _ = s.DB.Exec(`UPDATE printer_config SET reward_threshold_brl = ? WHERE id = 1`, val)
	_, err := s.DB.Exec(`UPDATE customers SET reward_threshold = ?`, val)
	return err
}

func parseAddressComponents(addressText string) (street, number, neighborhood, complement string) {
	clean := strings.TrimSpace(addressText)
	if clean == "" || strings.Contains(strings.ToUpper(clean), "RETIRADA") || strings.Contains(strings.ToUpper(clean), "SALÃO") || strings.Contains(strings.ToUpper(clean), "BALCÃO") {
		return "", "", "", ""
	}
	// Formato típico: "Rua Augusta, 2210 - Apto 42 - Jardins | GPS: ..."
	if idx := strings.Index(clean, "|"); idx >= 0 {
		clean = strings.TrimSpace(clean[:idx])
	}
	partsComma := strings.SplitN(clean, ",", 2)
	street = strings.TrimSpace(partsComma[0])
	if len(partsComma) > 1 {
		rest := strings.TrimSpace(partsComma[1])
		partsDash := strings.Split(rest, "-")
		if len(partsDash) > 0 {
			number = strings.TrimSpace(partsDash[0])
		}
		if len(partsDash) == 2 {
			neighborhood = strings.TrimSpace(partsDash[1])
		} else if len(partsDash) >= 3 {
			complement = strings.TrimSpace(partsDash[1])
			neighborhood = strings.TrimSpace(partsDash[len(partsDash)-1])
		}
	}
	return street, number, neighborhood, complement
}

func (s *Store) SyncCustomerFromOrder(o models.Order) {
	orig := strings.ToUpper(strings.TrimSpace(o.OriginType))
	isRetirada := orig == "RETIRADA" || strings.Contains(strings.ToUpper(o.HeaderTag), "RETIRADA") || strings.Contains(strings.ToUpper(o.AddressText), "RETIRADA")
	isDelivery := orig == "DELIVERY" && !isRetirada
	if !isDelivery && !isRetirada {
		return
	}

	cName := strings.TrimSpace(o.CustomerName)
	cPhone := whatsapp.NormalizeWhatsAppDigits(o.CustomerPhone)
	if cName == "" && cPhone == "" {
		return
	}
	if cName == "" {
		cName = "Cliente " + cPhone
	}
	channel := "DELIVERY"
	if isRetirada {
		channel = "RETIRADA"
	}

	street, number, neigh, comp := parseAddressComponents(o.AddressText)
	addrText := strings.TrimSpace(o.AddressText)
	if isRetirada && strings.Contains(strings.ToUpper(addrText), "RETIRADA") {
		addrText = ""
	}

	var existingID int
	var existingChannel, existingAddr, existingStreet, existingNumber, existingNeigh, existingComp string
	if cPhone != "" {
		_ = s.DB.QueryRow(`SELECT id, origin_channel, address_text, street, number, neighborhood, complement FROM customers WHERE phone = ? LIMIT 1`, cPhone).
			Scan(&existingID, &existingChannel, &existingAddr, &existingStreet, &existingNumber, &existingNeigh, &existingComp)
	}
	if existingID == 0 && cName != "" {
		_ = s.DB.QueryRow(`SELECT id, origin_channel, address_text, street, number, neighborhood, complement FROM customers WHERE LOWER(name) = LOWER(?) LIMIT 1`, cName).
			Scan(&existingID, &existingChannel, &existingAddr, &existingStreet, &existingNumber, &existingNeigh, &existingComp)
	}

	nowStr := o.CreatedAt
	if nowStr == "" {
		nowStr = time.Now().Format("02/01 15:04")
	}
	th := s.GetRewardThreshold()

	if existingID == 0 {
		_, _ = s.DB.Exec(`INSERT INTO customers (
			name, phone, origin_channel, street, number, neighborhood, complement, address_text,
			latitude, longitude, notes, total_spent, orders_count, rewards_redeemed, reward_threshold, last_order_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, 1, 0, ?, ?, ?)`,
			cName, cPhone, channel, street, number, neigh, comp, addrText,
			o.Latitude, o.Longitude, o.Total, th, nowStr, nowStr,
		)
	} else {
		newChannel := existingChannel
		if existingChannel != "" && existingChannel != channel {
			newChannel = "DELIVERY & RETIRADA"
		}
		if street == "" {
			street = existingStreet
		}
		if number == "" {
			number = existingNumber
		}
		if neigh == "" {
			neigh = existingNeigh
		}
		if comp == "" {
			comp = existingComp
		}
		if addrText == "" {
			addrText = existingAddr
		}
		_, _ = s.DB.Exec(`UPDATE customers SET
			name = ?, phone = CASE WHEN ? != '' THEN ? ELSE phone END, origin_channel = ?,
			street = ?, number = ?, neighborhood = ?, complement = ?, address_text = ?,
			latitude = CASE WHEN ? != 0 THEN ? ELSE latitude END,
			longitude = CASE WHEN ? != 0 THEN ? ELSE longitude END,
			last_order_at = ?
			WHERE id = ?`,
			cName, cPhone, cPhone, newChannel,
			street, number, neigh, comp, addrText,
			o.Latitude, o.Latitude, o.Longitude, o.Longitude,
			nowStr, existingID,
		)
	}
}

func (s *Store) SyncAllCustomersFromOrders() {
	orders, err := s.ListOrders("")
	if err != nil {
		return
	}
	// Processa do mais antigo para o mais recente
	for i := len(orders) - 1; i >= 0; i-- {
		s.SyncCustomerFromOrder(orders[i])
	}
}

func (s *Store) ListCustomers() ([]models.Customer, error) {
	th := s.GetRewardThreshold()
	allOrders, _ := s.ListOrders("")

	rows, err := s.DB.Query(`SELECT id, name, phone, origin_channel, street, number, neighborhood, complement, address_text, latitude, longitude, notes, total_spent, orders_count, rewards_redeemed, COALESCE(reward_threshold, 150.0), last_order_at, created_at FROM customers ORDER BY id DESC`)
	if err != nil {
		return []models.Customer{}, err
	}
	defer rows.Close()

	list := []models.Customer{}
	for rows.Next() {
		var c models.Customer
		if err := rows.Scan(
			&c.ID, &c.Name, &c.Phone, &c.OriginChannel, &c.Street, &c.Number, &c.Neighborhood, &c.Complement,
			&c.AddressText, &c.Latitude, &c.Longitude, &c.Notes, &c.TotalSpent, &c.OrdersCount,
			&c.RewardsRedeemed, &c.RewardThreshold, &c.LastOrderAt, &c.CreatedAt,
		); err == nil {
			c.RewardThreshold = th
			c.OrdersHistory = []models.Order{}
			var sumOrders float64
			var countOrders int
			normPhone := whatsapp.NormalizeWhatsAppDigits(c.Phone)

			for _, ord := range allOrders {
				if ord.Status == "CANCELADO" {
					continue
				}
				orig := strings.ToUpper(strings.TrimSpace(ord.OriginType))
				isRetirada := orig == "RETIRADA" || strings.Contains(strings.ToUpper(ord.HeaderTag), "RETIRADA") || strings.Contains(strings.ToUpper(ord.AddressText), "RETIRADA")
				isDelivery := orig == "DELIVERY" && !isRetirada
				if !isDelivery && !isRetirada {
					continue
				}
				ordPhone := whatsapp.NormalizeWhatsAppDigits(ord.CustomerPhone)
				phoneMatch := normPhone != "" && ordPhone != "" && normPhone == ordPhone
				nameMatch := strings.EqualFold(strings.TrimSpace(ord.CustomerName), strings.TrimSpace(c.Name))
				if phoneMatch || nameMatch {
					c.OrdersHistory = append(c.OrdersHistory, ord)
					sumOrders += ord.Total
					countOrders++
				}
			}
			c.OrdersCount = countOrders
			if countOrders > 0 {
				c.TotalSpent = math.Round(sumOrders*100) / 100
			} else {
				c.TotalSpent = 0
			}
			if c.AddressText == "" && c.Street != "" {
				c.AddressText = c.Street
				if c.Number != "" {
					c.AddressText += ", " + c.Number
				}
				if c.Complement != "" {
					c.AddressText += " - " + c.Complement
				}
				if c.Neighborhood != "" {
					c.AddressText += " - " + c.Neighborhood
				}
			}
			c.RewardsEarned = int(math.Floor(c.TotalSpent / c.RewardThreshold))
			c.RewardsAvailable = c.RewardsEarned - c.RewardsRedeemed
			if c.RewardsAvailable < 0 {
				c.RewardsAvailable = 0
			}
			c.ProgressToNext = math.Round(math.Mod(c.TotalSpent, c.RewardThreshold)*100) / 100
			c.RemainingToNext = math.Round((c.RewardThreshold-c.ProgressToNext)*100) / 100
			list = append(list, c)
		}
	}
	return list, nil
}

func (s *Store) SaveCustomer(c models.Customer) (*models.Customer, error) {
	c.Name = strings.TrimSpace(c.Name)
	c.Phone = whatsapp.NormalizeWhatsAppDigits(c.Phone)
	if c.Name == "" {
		return nil, fmt.Errorf("informe o nome do cliente")
	}
	if c.OriginChannel == "" {
		c.OriginChannel = "DELIVERY"
	}
	if c.Street != "" {
		built := c.Street
		if c.Number != "" {
			built += ", " + c.Number
		}
		if c.Complement != "" {
			built += " - " + c.Complement
		}
		if c.Neighborhood != "" {
			built += " - " + c.Neighborhood
		}
		c.AddressText = built
	}
	if c.Latitude == 0 {
		c.Latitude = -23.55052
	}
	if c.Longitude == 0 {
		c.Longitude = -46.63331
	}
	th := s.GetRewardThreshold()
	nowStr := time.Now().Format("02/01 15:04")

	if c.ID > 0 {
		_, err := s.DB.Exec(`UPDATE customers SET
			name=?, phone=?, origin_channel=?, street=?, number=?, neighborhood=?, complement=?,
			address_text=?, latitude=?, longitude=?, notes=?, total_spent=?, reward_threshold=?
			WHERE id=?`,
			c.Name, c.Phone, c.OriginChannel, c.Street, c.Number, c.Neighborhood, c.Complement,
			c.AddressText, c.Latitude, c.Longitude, c.Notes, c.TotalSpent, th, c.ID)
		if err != nil {
			return nil, err
		}
	} else {
		res, err := s.DB.Exec(`INSERT INTO customers (
			name, phone, origin_channel, street, number, neighborhood, complement,
			address_text, latitude, longitude, notes, total_spent, orders_count, rewards_redeemed, reward_threshold, last_order_at, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?, ?)`,
			c.Name, c.Phone, c.OriginChannel, c.Street, c.Number, c.Neighborhood, c.Complement,
			c.AddressText, c.Latitude, c.Longitude, c.Notes, c.TotalSpent, th, nowStr, nowStr)
		if err != nil {
			return nil, err
		}
		id64, _ := res.LastInsertId()
		c.ID = int(id64)
	}

	all, _ := s.ListCustomers()
	for _, item := range all {
		if item.ID == c.ID {
			return &item, nil
		}
	}
	return &c, nil
}

func (s *Store) AdminDeleteCustomer(customerID int) error {
	if customerID <= 0 {
		return fmt.Errorf("id de cliente inválido")
	}
	var cName, cPhone string
	err := s.DB.QueryRow(`SELECT COALESCE(name,''), COALESCE(phone,'') FROM customers WHERE id = ?`, customerID).Scan(&cName, &cPhone)
	if err != nil {
		return fmt.Errorf("cliente #%d não encontrado", customerID)
	}
	_, err = s.DB.Exec(`DELETE FROM customers WHERE id = ?`, customerID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cPhone) != "" {
		_, _ = s.DB.Exec(`DELETE FROM customers WHERE phone = ?`, strings.TrimSpace(cPhone))
	}
	if strings.TrimSpace(cName) != "" {
		_, _ = s.DB.Exec(`DELETE FROM customers WHERE LOWER(TRIM(name)) = LOWER(TRIM(?))`, strings.TrimSpace(cName))
	}
	return nil
}

func (s *Store) RedeemCustomerReward(customerID int) (*models.Customer, error) {
	all, _ := s.ListCustomers()
	var target *models.Customer
	for i := range all {
		if all[i].ID == customerID {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("cliente não encontrado")
	}
	if target.RewardsAvailable <= 0 {
		return nil, fmt.Errorf("este cliente ainda não possui brindes disponíveis para resgate (faltam R$ %.2f)", target.RemainingToNext)
	}
	_, err := s.DB.Exec(`UPDATE customers SET rewards_redeemed = rewards_redeemed + 1 WHERE id = ?`, customerID)
	if err != nil {
		return nil, err
	}
	after, _ := s.ListCustomers()
	for _, item := range after {
		if item.ID == customerID {
			return &item, nil
		}
	}
	return target, nil
}

// =========================================================================
// ALARMES DO GARÇOM (CHAMADO DE MESA, BEBIDAS SEM IMPRESSORA E PEDIDO PRONTO NA COZINHA)
// =========================================================================
func isDrinkText(s string) bool {
	lower := strings.ToLower(s)
	keywords := []string{
		"bebida", "refrigerante", "coca", "guaran", "fanta", "sprite", "pepsi",
		"cerveja", "heineken", "stella", "budweiser", "corona", "spaten", "original", "brahma",
		"chopp", "chope", "suco", "água", "agua", "h2oh", "schweppes", "tônica", "tonica",
		"energético", "energetico", "red bull", "monster", "drink", "caipirinha", "gin", "whisky", "vinho", "lata", "long neck", "600ml", "2l",
	}
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

func (s *Store) extractDrinkItems(items []models.OrderItem) []models.OrderItem {
	routes, _ := s.ListCategoryPrinterRoutes()
	routeMap := map[int]string{}
	for _, r := range routes {
		routeMap[r.CategoryID] = r.PrinterStationID
	}
	var drinks []models.OrderItem
	for _, it := range items {
		var catID int
		var catName, sector string
		_ = s.DB.QueryRow(`SELECT category_id, category_name, sector FROM products WHERE id = ?`, it.ProductID).Scan(&catID, &catName, &sector)
		stID := routeMap[catID]
		if stID == "BAR_COPA" || isDrinkText(catName+" "+it.ProductName) || strings.EqualFold(it.SectorABC, "MERCADO_BEBIDAS") {
			drinks = append(drinks, it)
		}
	}
	return drinks
}

func (s *Store) CreateWaiterAlert(a models.WaiterAlert) (*models.WaiterAlert, error) {
	if a.AlertType == "" {
		a.AlertType = "CHAMADO_MESA"
	}
	a.Status = "PENDENTE"
	a.CreatedAt = time.Now().Format("15:04:05")
	if a.TableID > 0 && a.TableLabel == "" {
		_ = s.DB.QueryRow(`SELECT number, label FROM dining_tables WHERE id = ? OR number = ? LIMIT 1`, a.TableID, a.TableID).Scan(&a.TableNumber, &a.TableLabel)
		if a.TableLabel == "" {
			a.TableNumber = a.TableID
			a.TableLabel = fmt.Sprintf("MESA %02d", a.TableID)
		}
	}
	// Se for CHAMADO_MESA e já houver um pendente para a mesma mesa, apenas atualiza o horário/título
	if a.AlertType == "CHAMADO_MESA" && a.TableID > 0 {
		var existingID int
		_ = s.DB.QueryRow(`SELECT id FROM waiter_alerts WHERE alert_type = 'CHAMADO_MESA' AND table_id = ? AND status = 'PENDENTE' LIMIT 1`, a.TableID).Scan(&existingID)
		if existingID > 0 {
			_, _ = s.DB.Exec(`UPDATE waiter_alerts SET title = ?, details = ?, created_at = ? WHERE id = ?`, a.Title, a.Details, a.CreatedAt, existingID)
			a.ID = existingID
			return &a, nil
		}
	}

	itemsJSON, _ := json.Marshal(a.Items)
	res, err := s.DB.Exec(`INSERT INTO waiter_alerts (
		alert_type, table_id, table_number, table_label, order_id, order_code, comanda_code, customer_name,
		title, details, items_json, status, created_at, confirmed_by, confirmed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'PENDENTE', ?, '', '')`,
		a.AlertType, a.TableID, a.TableNumber, a.TableLabel, a.OrderID, a.OrderCode, a.ComandaCode, a.CustomerName,
		a.Title, a.Details, string(itemsJSON), a.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	id64, _ := res.LastInsertId()
	a.ID = int(id64)
	return &a, nil
}

func (s *Store) TriggerKitchenReadyAlert(orderID int) (*models.WaiterAlert, error) {
	cfg := s.GetPrinterConfig()
	if !cfg.WaiterAlarmKitchenReady {
		return nil, fmt.Errorf("o alarme de Pedido Pronto na Cozinha está desativado pelo ADM")
	}
	ord, err := s.GetOrderByIDOrQR(strconv.Itoa(orderID))
	if err != nil || ord == nil {
		return nil, fmt.Errorf("pedido #%d não encontrado", orderID)
	}
	tID := 0
	tNum := 0
	tLbl := ord.HeaderTag
	if ord.TableID != nil && *ord.TableID > 0 {
		tID = *ord.TableID
		_ = s.DB.QueryRow(`SELECT number, label FROM dining_tables WHERE id = ? OR number = ? LIMIT 1`, tID, tID).Scan(&tNum, &tLbl)
	}
	if tLbl == "" {
		if tID > 0 {
			tLbl = fmt.Sprintf("MESA %02d", tID)
		} else {
			tLbl = ord.HeaderTag
		}
	}
	var itemLines []string
	for _, it := range ord.Items {
		qStr := fmt.Sprintf("%.0fx", it.Quantity)
		line := fmt.Sprintf("%s %s", qStr, it.ProductName)
		var mods []string
		for _, m := range it.SelectedModifiers {
			mods = append(mods, m.OptionName)
		}
		if len(mods) > 0 {
			line += " (" + strings.Join(mods, ", ") + ")"
		}
		if strings.TrimSpace(it.Notes) != "" {
			line += " [Obs: " + strings.TrimSpace(it.Notes) + "]"
		}
		itemLines = append(itemLines, line)
	}
	return s.CreateWaiterAlert(models.WaiterAlert{
		AlertType:    "PRONTO_COZINHA",
		TableID:      tID,
		TableNumber:  tNum,
		TableLabel:   tLbl,
		OrderID:      ord.ID,
		OrderCode:    ord.OrderCode,
		ComandaCode:  ord.ComandaCode,
		CustomerName: ord.CustomerName,
		Title:        fmt.Sprintf("🍔 %s — PEDIDO %s PRONTO NA COZINHA!", tLbl, ord.OrderCode),
		Details:      strings.Join(itemLines, " • "),
		Items:        ord.Items,
	})
}

func (s *Store) ListWaiterAlerts(includeConfirmed bool) ([]models.WaiterAlert, error) {
	cfg := s.GetPrinterConfig()
	query := `SELECT id, alert_type, table_id, table_number, table_label, order_id, order_code, comanda_code, customer_name,
		title, details, items_json, status, created_at, confirmed_by, confirmed_at
		FROM waiter_alerts`
	if !includeConfirmed {
		query += ` WHERE status = 'PENDENTE'`
	}
	query += ` ORDER BY id DESC LIMIT 60`

	rows, err := s.DB.Query(query)
	if err != nil {
		return []models.WaiterAlert{}, err
	}
	defer rows.Close()

	list := []models.WaiterAlert{}
	for rows.Next() {
		var a models.WaiterAlert
		var itemsJSON string
		if err := rows.Scan(
			&a.ID, &a.AlertType, &a.TableID, &a.TableNumber, &a.TableLabel, &a.OrderID, &a.OrderCode, &a.ComandaCode, &a.CustomerName,
			&a.Title, &a.Details, &itemsJSON, &a.Status, &a.CreatedAt, &a.ConfirmedBy, &a.ConfirmedAt,
		); err == nil {
			// Filtra caso o ADM tenha desativado algum dos 3 recursos
			if !includeConfirmed {
				if a.AlertType == "CHAMADO_MESA" && !cfg.WaiterAlarmTableCall {
					continue
				}
				if a.AlertType == "BEBIDAS_MESA" && !cfg.WaiterAlarmDrinks {
					continue
				}
				if a.AlertType == "PRONTO_COZINHA" && !cfg.WaiterAlarmKitchenReady {
					continue
				}
			}
			_ = json.Unmarshal([]byte(itemsJSON), &a.Items)
			if a.Items == nil {
				a.Items = []models.OrderItem{}
			}
			list = append(list, a)
		}
	}
	return list, nil
}

func (s *Store) ConfirmWaiterAlert(alertID int, tableID int, alertType string, confirmedBy string) error {
	if confirmedBy == "" {
		confirmedBy = "Garçom"
	}
	now := time.Now().Format("15:04:05")
	if alertID > 0 {
		var tID int
		var aType string
		_ = s.DB.QueryRow(`SELECT table_id, alert_type FROM waiter_alerts WHERE id = ?`, alertID).Scan(&tID, &aType)
		_, err := s.DB.Exec(`UPDATE waiter_alerts SET status = 'CONFIRMADO', confirmed_by = ?, confirmed_at = ? WHERE id = ?`, confirmedBy, now, alertID)
		if aType == "CHAMADO_MESA" && tID > 0 {
			_, _ = s.DB.Exec(`UPDATE dining_tables SET waiter_call = 0, updated_at = ? WHERE id = ?`, time.Now().Format("15:04"), tID)
		}
		return err
	}
	if tableID > 0 {
		if alertType != "" {
			_, err := s.DB.Exec(`UPDATE waiter_alerts SET status = 'CONFIRMADO', confirmed_by = ?, confirmed_at = ? WHERE table_id = ? AND alert_type = ? AND status = 'PENDENTE'`, confirmedBy, now, tableID, alertType)
			return err
		}
		_, err := s.DB.Exec(`UPDATE waiter_alerts SET status = 'CONFIRMADO', confirmed_by = ?, confirmed_at = ? WHERE table_id = ? AND status = 'PENDENTE'`, confirmedBy, now, tableID)
		return err
	}
	return nil
}

// =========================================================================
// SISTEMA DE CUPONS DE DESCONTO (FIXOS ATIVAR/DESATIVAR E USO ÚNICO P/ CLIENTES)
// =========================================================================
func normalizeCouponCode(raw string) string {
	clean := strings.ToUpper(strings.TrimSpace(raw))
	clean = strings.ReplaceAll(clean, " ", "-")
	return clean
}

func (s *Store) ListDiscountCoupons() ([]models.DiscountCoupon, error) {
	rows, err := s.DB.Query(`SELECT id, code, coupon_type, discount_type, discount_value, customer_id, customer_name, customer_phone, reason, active, used_count, used_at, used_order_id, created_at
		FROM discount_coupons ORDER BY id DESC`)
	if err != nil {
		return []models.DiscountCoupon{}, err
	}
	defer rows.Close()

	list := []models.DiscountCoupon{}
	for rows.Next() {
		var c models.DiscountCoupon
		var activeInt int
		if err := rows.Scan(
			&c.ID, &c.Code, &c.CouponType, &c.DiscountType, &c.DiscountValue,
			&c.CustomerID, &c.CustomerName, &c.CustomerPhone, &c.Reason,
			&activeInt, &c.UsedCount, &c.UsedAt, &c.UsedOrderID, &c.CreatedAt,
		); err == nil {
			c.Active = activeInt == 1
			if strings.EqualFold(c.CouponType, "USO_UNICO") && c.UsedCount >= 1 {
				c.Active = false
			}
			list = append(list, c)
		}
	}
	return list, nil
}

func (s *Store) SaveDiscountCoupon(c models.DiscountCoupon) (*models.DiscountCoupon, error) {
	c.Code = normalizeCouponCode(c.Code)
	if c.Code == "" {
		return nil, fmt.Errorf("informe o código do cupom (ex: SIRBEEF10 ou VIP-CLIENTE)")
	}
	c.CouponType = strings.ToUpper(strings.TrimSpace(c.CouponType))
	if c.CouponType != "USO_UNICO" {
		c.CouponType = "FIXO"
	}
	c.DiscountType = strings.ToUpper(strings.TrimSpace(c.DiscountType))
	if c.DiscountType != "PERCENTUAL" {
		c.DiscountType = "VALOR"
	}
	if c.DiscountValue <= 0 {
		return nil, fmt.Errorf("o valor/percentual de desconto deve ser maior que zero")
	}
	if c.DiscountType == "PERCENTUAL" && c.DiscountValue > 100 {
		c.DiscountValue = 100
	}
	c.CustomerName = strings.TrimSpace(c.CustomerName)
	c.CustomerPhone = whatsapp.NormalizeWhatsAppDigits(c.CustomerPhone)
	c.Reason = strings.TrimSpace(c.Reason)
	now := time.Now().Format("02/01/2006 15:04")

	activeInt := 1
	if !c.Active {
		activeInt = 0
	}

	if c.ID > 0 {
		_, err := s.DB.Exec(`UPDATE discount_coupons SET
			code = ?, coupon_type = ?, discount_type = ?, discount_value = ?,
			customer_id = ?, customer_name = ?, customer_phone = ?, reason = ?, active = ?
			WHERE id = ?`,
			c.Code, c.CouponType, c.DiscountType, c.DiscountValue,
			c.CustomerID, c.CustomerName, c.CustomerPhone, c.Reason, activeInt, c.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("erro ao atualizar cupom (verifique se o código já existe): %w", err)
		}
	} else {
		var existingID int
		_ = s.DB.QueryRow(`SELECT id FROM discount_coupons WHERE UPPER(code) = UPPER(?) LIMIT 1`, c.Code).Scan(&existingID)
		if existingID > 0 {
			return nil, fmt.Errorf("já existe um cupom cadastrado com o código '%s'", c.Code)
		}
		res, err := s.DB.Exec(`INSERT INTO discount_coupons (
			code, coupon_type, discount_type, discount_value, customer_id, customer_name, customer_phone, reason, active, used_count, used_at, used_order_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '', 0, ?)`,
			c.Code, c.CouponType, c.DiscountType, c.DiscountValue,
			c.CustomerID, c.CustomerName, c.CustomerPhone, c.Reason, activeInt, now,
		)
		if err != nil {
			return nil, err
		}
		lastID, _ := res.LastInsertId()
		c.ID = int(lastID)
		c.CreatedAt = now
	}

	all, _ := s.ListDiscountCoupons()
	for _, item := range all {
		if item.ID == c.ID {
			return &item, nil
		}
	}
	return &c, nil
}

func (s *Store) ToggleDiscountCoupon(id int, active bool) (*models.DiscountCoupon, error) {
	if id <= 0 {
		return nil, fmt.Errorf("ID de cupom inválido")
	}
	var cType string
	var usedCnt int
	err := s.DB.QueryRow(`SELECT coupon_type, used_count FROM discount_coupons WHERE id = ?`, id).Scan(&cType, &usedCnt)
	if err != nil {
		return nil, fmt.Errorf("cupom não encontrado")
	}
	if strings.EqualFold(cType, "USO_UNICO") && usedCnt >= 1 && active {
		return nil, fmt.Errorf("este cupom é de Uso Único e já foi utilizado pelo cliente")
	}
	activeInt := 0
	if active {
		activeInt = 1
	}
	_, err = s.DB.Exec(`UPDATE discount_coupons SET active = ? WHERE id = ?`, activeInt, id)
	if err != nil {
		return nil, err
	}
	all, _ := s.ListDiscountCoupons()
	for _, item := range all {
		if item.ID == id {
			return &item, nil
		}
	}
	return nil, nil
}

func (s *Store) DeleteDiscountCoupon(id int) error {
	if id <= 0 {
		return fmt.Errorf("ID de cupom inválido")
	}
	_, err := s.DB.Exec(`DELETE FROM discount_coupons WHERE id = ?`, id)
	return err
}

func (s *Store) ValidateDiscountCoupon(code string, subtotal float64, customerPhone string) (*models.DiscountCoupon, float64, error) {
	cleanCode := normalizeCouponCode(code)
	if cleanCode == "" {
		return nil, 0, fmt.Errorf("informe o código do cupom")
	}

	var c models.DiscountCoupon
	var activeInt int
	err := s.DB.QueryRow(`SELECT id, code, coupon_type, discount_type, discount_value, customer_id, customer_name, customer_phone, reason, active, used_count, used_at, used_order_id, created_at
		FROM discount_coupons WHERE UPPER(code) = UPPER(?) LIMIT 1`, cleanCode).Scan(
		&c.ID, &c.Code, &c.CouponType, &c.DiscountType, &c.DiscountValue,
		&c.CustomerID, &c.CustomerName, &c.CustomerPhone, &c.Reason,
		&activeInt, &c.UsedCount, &c.UsedAt, &c.UsedOrderID, &c.CreatedAt,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("cupom '%s' não encontrado", cleanCode)
	}
	c.Active = activeInt == 1

	if strings.EqualFold(c.CouponType, "USO_UNICO") && c.UsedCount >= 1 {
		if c.UsedAt != "" {
			return nil, 0, fmt.Errorf("o cupom '%s' é de Uso Único e já foi utilizado em %s", c.Code, c.UsedAt)
		}
		return nil, 0, fmt.Errorf("o cupom '%s' é de Uso Único e já foi utilizado", c.Code)
	}
	if !c.Active {
		return nil, 0, fmt.Errorf("o cupom '%s' está desativado no momento", c.Code)
	}

	var calcDiscount float64
	if strings.EqualFold(c.DiscountType, "PERCENTUAL") {
		if subtotal > 0 {
			calcDiscount = math.Round((subtotal*(c.DiscountValue/100.0))*100) / 100
		}
	} else {
		calcDiscount = math.Round(c.DiscountValue*100) / 100
	}
	if subtotal > 0 && calcDiscount > subtotal {
		calcDiscount = subtotal
	}
	return &c, calcDiscount, nil
}

func (s *Store) MarkDiscountCouponUsed(code string, orderID int) {
	cleanCode := normalizeCouponCode(code)
	if cleanCode == "" {
		return
	}
	now := time.Now().Format("02/01/2006 15:04")
	_, _ = s.DB.Exec(`UPDATE discount_coupons SET
		used_count = used_count + 1,
		used_at = ?,
		used_order_id = ?,
		active = CASE WHEN UPPER(coupon_type) = 'USO_UNICO' THEN 0 ELSE active END
		WHERE UPPER(code) = UPPER(?)`,
		now, orderID, cleanCode,
	)
}

// ApplyCouponToComandaOrOrder aplica um cupom de desconto ao fechar uma Comanda, Mesa ou Retirada no Caixa Geral,
// abatendo o valor do pedido ativo e queimando o cupom caso seja de Uso Único.
func (s *Store) ApplyCouponToComandaOrOrder(comandaID int, tableID int, orderID int, couponCode string, discountAmount float64) {
	cleanCode := normalizeCouponCode(couponCode)
	if cleanCode == "" {
		return
	}
	targetOrderID := orderID
	var ordSub, ordDeliv, ordTot float64
	var custPhone string

	if targetOrderID <= 0 && comandaID > 0 {
		_ = s.DB.QueryRow(`SELECT id, subtotal, delivery_fee, total, COALESCE(customer_phone,'') FROM orders WHERE comanda_id = ? AND status != 'CANCELADO' ORDER BY total DESC, id DESC LIMIT 1`, comandaID).
			Scan(&targetOrderID, &ordSub, &ordDeliv, &ordTot, &custPhone)
	}
	if targetOrderID <= 0 && tableID > 0 {
		_ = s.DB.QueryRow(`SELECT id, subtotal, delivery_fee, total, COALESCE(customer_phone,'') FROM orders WHERE table_id = ? AND status != 'CANCELADO' ORDER BY total DESC, id DESC LIMIT 1`, tableID).
			Scan(&targetOrderID, &ordSub, &ordDeliv, &ordTot, &custPhone)
	}
	if targetOrderID > 0 && ordSub <= 0 && ordTot <= 0 {
		_ = s.DB.QueryRow(`SELECT subtotal, delivery_fee, total, COALESCE(customer_phone,'') FROM orders WHERE id = ?`, targetOrderID).
			Scan(&ordSub, &ordDeliv, &ordTot, &custPhone)
	}
	if targetOrderID <= 0 {
		return
	}

	var totalBase float64
	if comandaID > 0 {
		_ = s.DB.QueryRow(`SELECT COALESCE(SUM(total), 0) FROM orders WHERE comanda_id = ? AND status != 'CANCELADO'`, comandaID).Scan(&totalBase)
	} else if tableID > 0 {
		_ = s.DB.QueryRow(`SELECT COALESCE(SUM(total), 0) FROM orders WHERE table_id = ? AND status != 'CANCELADO'`, tableID).Scan(&totalBase)
	} else {
		totalBase = ordSub + ordDeliv
	}

	if discountAmount <= 0 {
		if _, calcDisc, err := s.ValidateDiscountCoupon(cleanCode, totalBase, custPhone); err == nil && calcDisc > 0 {
			discountAmount = calcDisc
		}
	}
	if discountAmount <= 0 {
		return
	}
	if discountAmount > (ordSub + ordDeliv) {
		discountAmount = ordSub + ordDeliv
	}
	newOrdTot := math.Max(0, math.Round((ordSub+ordDeliv-discountAmount)*100)/100)
	_, _ = s.DB.Exec(`UPDATE orders SET coupon_code = ?, discount_amount = ?, total = ? WHERE id = ?`,
		cleanCode, math.Round(discountAmount*100)/100, newOrdTot, targetOrderID)

	s.MarkDiscountCouponUsed(cleanCode, targetOrderID)
	if comandaID > 0 || tableID > 0 {
		s.recalculateComandaAndTable(comandaID, tableID)
	}
	s.RecalcCashierShiftTotals()
}

func (s *Store) UpdateMotoboyLocation(loc models.MotoboyLiveLocation) error {
	if loc.MotoboyID <= 0 {
		loc.MotoboyID = 4
	}
	if strings.TrimSpace(loc.MotoboyName) == "" {
		loc.MotoboyName = "Rafael (Motoboy Express 01)"
	}
	nowStr := time.Now().Format("02/01/2006 15:04:05")
	nowUnix := time.Now().Unix()
	_, err := s.DB.Exec(`INSERT INTO motoboy_locations (
		motoboy_id, motoboy_name, latitude, longitude, accuracy, speed, heading, updated_at, updated_at_unix
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(motoboy_id) DO UPDATE SET
		motoboy_name = excluded.motoboy_name,
		latitude = excluded.latitude,
		longitude = excluded.longitude,
		accuracy = excluded.accuracy,
		speed = excluded.speed,
		heading = excluded.heading,
		updated_at = excluded.updated_at,
		updated_at_unix = excluded.updated_at_unix`,
		loc.MotoboyID, strings.TrimSpace(loc.MotoboyName), loc.Latitude, loc.Longitude,
		loc.Accuracy, loc.Speed, loc.Heading, nowStr, nowUnix,
	)
	return err
}

func (s *Store) GetMotoboyLocation(motoboyID int, motoboyName string) *models.MotoboyLiveLocation {
	var loc models.MotoboyLiveLocation
	var err error
	if motoboyID > 0 {
		err = s.DB.QueryRow(`SELECT motoboy_id, motoboy_name, latitude, longitude, accuracy, speed, heading, updated_at, updated_at_unix
			FROM motoboy_locations WHERE motoboy_id = ? LIMIT 1`, motoboyID).Scan(
			&loc.MotoboyID, &loc.MotoboyName, &loc.Latitude, &loc.Longitude,
			&loc.Accuracy, &loc.Speed, &loc.Heading, &loc.UpdatedAt, &loc.UpdatedAtUnix,
		)
	}
	if (err != nil || loc.MotoboyID == 0) && strings.TrimSpace(motoboyName) != "" {
		err = s.DB.QueryRow(`SELECT motoboy_id, motoboy_name, latitude, longitude, accuracy, speed, heading, updated_at, updated_at_unix
			FROM motoboy_locations WHERE LOWER(motoboy_name) = LOWER(?) ORDER BY updated_at_unix DESC LIMIT 1`, strings.TrimSpace(motoboyName)).Scan(
			&loc.MotoboyID, &loc.MotoboyName, &loc.Latitude, &loc.Longitude,
			&loc.Accuracy, &loc.Speed, &loc.Heading, &loc.UpdatedAt, &loc.UpdatedAtUnix,
		)
	}
	if err != nil || loc.MotoboyID == 0 {
		// Fallback para a localização mais recente se só houver 1 motoboy transmitindo
		err = s.DB.QueryRow(`SELECT motoboy_id, motoboy_name, latitude, longitude, accuracy, speed, heading, updated_at, updated_at_unix
			FROM motoboy_locations ORDER BY updated_at_unix DESC LIMIT 1`).Scan(
			&loc.MotoboyID, &loc.MotoboyName, &loc.Latitude, &loc.Longitude,
			&loc.Accuracy, &loc.Speed, &loc.Heading, &loc.UpdatedAt, &loc.UpdatedAtUnix,
		)
	}
	if err != nil || loc.MotoboyID == 0 || (loc.Latitude == 0 && loc.Longitude == 0) {
		return nil
	}
	nowUnix := time.Now().Unix()
	if loc.UpdatedAtUnix > 0 {
		loc.SecondsAgo = nowUnix - loc.UpdatedAtUnix
		if loc.SecondsAgo < 0 {
			loc.SecondsAgo = 0
		}
		loc.IsLive = loc.SecondsAgo <= 180 // até 3 minutos considerado sinal GPS ao vivo
	}
	return &loc
}



