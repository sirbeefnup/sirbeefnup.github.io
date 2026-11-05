package db

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed market_products.json
var marketProductsJSON []byte

type legacyMarketItem struct {
	Code  string  `json:"code"`
	Name  string  `json:"name"`
	Price float64 `json:"price"`
}

// EnsureLegacyMarketProducts garante a importação e atualização contínua de todos os 814 produtos
// da Boutique das Carnes & Empório tanto no servidor local de teste quanto no PC da Hamburgueria.
func (s *Store) EnsureLegacyMarketProducts() error {
	if len(marketProductsJSON) == 0 {
		return nil
	}

	var items []legacyMarketItem
	if err := json.Unmarshal(marketProductsJSON, &items); err != nil {
		return fmt.Errorf("erro ao decodificar market_products.json: %w", err)
	}

	// 1. Garante que a categoria 900 exista com os setores corretos
	_, _ = s.DB.Exec(`INSERT OR IGNORE INTO categories (id, name, sector, sector_abc)
		VALUES (900, 'Boutique das Carnes & Empório', 'MERCADO', 'MERCADO_CARNES')`)
	_, _ = s.DB.Exec(`UPDATE categories SET name = 'Boutique das Carnes & Empório', sector = 'MERCADO', sector_abc = 'MERCADO_CARNES' WHERE id = 900`)

	// 2. Garante a rota de impressão padrão para categoria 900 (BALCAO / CAIXA)
	var routeCount int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM category_printer_routes WHERE category_id = 900`).Scan(&routeCount)
	if routeCount == 0 {
		_, _ = s.DB.Exec(`INSERT INTO category_printer_routes (category_id, category_name, printer_station_id)
			VALUES (900, 'Boutique das Carnes & Empório', 'BALCAO')`)
	}

	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	checkStmt, err := tx.Prepare(`SELECT id FROM products WHERE ean13 = ? OR (plu_code = ? AND plu_code != '') LIMIT 1`)
	if err != nil {
		return err
	}
	defer checkStmt.Close()

	insertStmt, err := tx.Prepare(`INSERT INTO products (
		category_id, category_name, sector, sector_abc, name, description,
		price, promo_from, cost_price, ean13, plu_code, is_weighted,
		ncm, cest, cfop, image_url, modifier_groups_json, stock_qty, min_stock, track_stock, active
	) VALUES (
		900, 'Boutique das Carnes & Empório', 'MERCADO', 'MERCADO_CARNES', ?, '',
		?, NULL, 0, ?, ?, 0,
		'21069090', '1709900', '5102', '', '[]', -1, 5.0, 0, 1
	)`)
	if err != nil {
		return err
	}
	defer insertStmt.Close()

	updateStmt, err := tx.Prepare(`UPDATE products SET 
		name = ?, price = ?, category_id = 900, category_name = 'Boutique das Carnes & Empório',
		sector = 'MERCADO', sector_abc = 'MERCADO_CARNES', active = 1
		WHERE id = ?`)
	if err != nil {
		return err
	}
	defer updateStmt.Close()

	for _, it := range items {
		code := strings.TrimSpace(it.Code)
		name := strings.TrimSpace(it.Name)
		if code == "" || name == "" {
			continue
		}
		plu := ""
		if len(code) <= 6 {
			plu = code
		}

		var existingID int
		err := checkStmt.QueryRow(code, code).Scan(&existingID)
		if err == nil && existingID > 0 {
			_, _ = updateStmt.Exec(name, it.Price, existingID)
		} else {
			_, _ = insertStmt.Exec(name, it.Price, code, plu)
		}
	}

	return tx.Commit()
}
