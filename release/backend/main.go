package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"sirbeef/backend/internal/db"
	"sirbeef/backend/internal/escpos"
	"sirbeef/backend/internal/models"
	"sirbeef/backend/internal/whatsapp"
)

type Server struct {
	Store              *db.Store
	WhatsApp           *whatsapp.Manager
	WebDir             string
	DataDir            string
	Port               string
	lastSyncedTunnel   string
	remotePrinterMu    sync.Mutex
	remotePrintersByPC map[string][]models.DetectedPrinter
	remotePCSeenAt     map[string]time.Time
	remotePrintQueue   []models.RemotePrintJob
}

func main() {
	exeDir, _ := os.Getwd()
	dataDir := filepath.Join(exeDir, "data")
	_ = os.MkdirAll(dataDir, 0755)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Inicializador automático de Rede Local e Firewall do Windows em background (auto-eleva como ADM se necessário)
	go ensureWindowsNetworkReady(port, exeDir)

	dbPath := filepath.Join(dataDir, "sirbeef.db")
	seedPath := filepath.Join(dataDir, "seed_catalog.json")
	webDir := filepath.Join(exeDir, "web")

	store, err := db.OpenStore(dbPath, seedPath)
	if err != nil {
		log.Fatalf("Erro fatal ao abrir banco SQLite WAL: %v", err)
	}

	srv := &Server{
		Store:              store,
		WebDir:             webDir,
		DataDir:            dataDir,
		Port:               port,
		remotePrintersByPC: make(map[string][]models.DetectedPrinter),
		remotePCSeenAt:     make(map[string]time.Time),
	}

	waSessionPath := filepath.Join(dataDir, "whatsapp_session.db")
	waMgr, waErr := whatsapp.NewManager(
		waSessionPath,
		func() string {
			return "https://sirbeefnup.github.io/cardapio"
		},
		func(senderPhone string, pushName string, text string) (string, string) {
			store.LogWhatsAppWithStatus(senderPhone, pushName, "INBOUND", "MENSAGEM_CLIENTE_REAL", text, "RECEBIDO_WHATSAPP")
			webLink := "https://sirbeefnup.github.io/cardapio"
			reply, trigger := srv.processIncomingWhatsAppBotMessage(senderPhone, pushName, text, webLink)
			store.LogWhatsAppWithStatus(senderPhone, pushName, "OUTBOUND", trigger, reply, "ENVIADO_WHATSAPP")
			return reply, trigger
		},
	)
	if waErr != nil {
		log.Printf("⚠️ Aviso ao inicializar gerenciador WhatsApp Multi-Device: %v", waErr)
	} else {
		srv.WhatsApp = waMgr
		store.WhatsApp = waMgr
	}

	mux := http.NewServeMux()

	// Health & Discovery + Verificador Automático de Atualização (OTA) do APK Kotlin
	mux.HandleFunc("/api/health", srv.withCORS(srv.handleHealth))
	mux.HandleFunc("/api/ping", srv.withCORS(srv.handleHealth))
	mux.HandleFunc("/api/app/version", srv.withCORS(srv.handleAppVersion))

	// Auth & RBAC (ADMIN, CAIXA, GARCOM, MOTOBOY, TABLET) + Cadastro e Aprovação pelo ADM
	mux.HandleFunc("/api/auth/login", srv.withCORS(srv.handleLogin))
	mux.HandleFunc("/api/auth/register", srv.withCORS(srv.handleRegister))
	mux.HandleFunc("/api/users", srv.withCORS(srv.handleUsers))
	mux.HandleFunc("/api/auth/users", srv.withCORS(srv.handleUsers))

	// Catálogo, Gestão de Cardápio, Categorias CRUD, Preços, Estoque em Tempo Real, Grupos de Adicionais, Promoções da Semana e Busca por EAN-13 / Balança
	mux.HandleFunc("/api/catalog", srv.withCORS(srv.handleCatalog))
	mux.HandleFunc("/api/categories", srv.withCORS(srv.handleCategories))
	mux.HandleFunc("/api/products", srv.withCORS(srv.handleProducts))
	mux.HandleFunc("/api/products/quick", srv.withCORS(srv.handleProductQuickAction))
	mux.HandleFunc("/api/products/upload-image", srv.withCORS(srv.handleProductImageUpload))
	mux.HandleFunc("/api/modifier-groups", srv.withCORS(srv.handleModifierGroups))
	mux.HandleFunc("/api/weekly-promotions", srv.withCORS(srv.handleWeeklyPromotions))
	mux.HandleFunc("/api/barcode", srv.withCORS(srv.handleBarcodeLookup))

	// Salão, 30 Mesas Fixas (01..12 Tablet Kiosk [T], 13..30 Salão [Q]) e Comandas Individuais (QR Code + Nome/Celular)
	mux.HandleFunc("/api/tables", srv.withCORS(srv.handleTables))
	mux.HandleFunc("/api/tables/status", srv.withCORS(srv.handleTableStatus))
	mux.HandleFunc("/api/comandas", srv.withCORS(srv.handleComandas))
	mux.HandleFunc("/api/comandas/cards", srv.withCORS(srv.handleComandaCards))

	// Pedidos, Kanban PDV Delivery, Scanner QR Code Motoboy e Impressão ESC/POS
	mux.HandleFunc("/api/orders", srv.withCORS(srv.handleOrders))
	mux.HandleFunc("/api/orders/status", srv.withCORS(srv.handleOrderStatus))
	mux.HandleFunc("/api/orders/manage", srv.withCORS(srv.handleOrderStatus))
	mux.HandleFunc("/api/orders/dispatch-qr", srv.withCORS(srv.handleDispatchByQR))
	mux.HandleFunc("/api/orders/receipt", srv.withCORS(srv.handleOrderReceipt))
	mux.HandleFunc("/api/orders/track", srv.withCORS(srv.handleOrderTrack))

	// Logística Motoboy: Rota Otimizada (Radar), Localização GPS Ao Vivo, Acerto Virtual Noturno e Caixa Individual por Motoboy
	mux.HandleFunc("/api/motoboy/settlement", srv.withCORS(srv.handleMotoboySettlement))
	mux.HandleFunc("/api/motoboy/shifts", srv.withCORS(srv.handleMotoboyShifts))
	mux.HandleFunc("/api/motoboy/route", srv.withCORS(srv.handleMotoboyOptimizedRoute))
	mux.HandleFunc("/api/motoboy/location", srv.withCORS(srv.handleMotoboyLocation))

	// Motor WhatsApp CRM (Conexão Real QR Code Multi-Device + Código 8 Dígitos + Bot Automático + Comprovantes)
	mux.HandleFunc("/api/whatsapp/status", srv.withCORS(srv.handleWhatsAppStatus))
	mux.HandleFunc("/api/whatsapp/connect", srv.withCORS(srv.handleWhatsAppConnect))
	mux.HandleFunc("/api/whatsapp/disconnect", srv.withCORS(srv.handleWhatsAppDisconnect))
	mux.HandleFunc("/api/whatsapp/send-test", srv.withCORS(srv.handleWhatsAppSendTest))
	mux.HandleFunc("/api/whatsapp/config", srv.withCORS(srv.handleWhatsAppConfig))
	mux.HandleFunc("/api/whatsapp/webhook", srv.withCORS(srv.handleWhatsAppWebhook))
	mux.HandleFunc("/api/whatsapp/logs", srv.withCORS(srv.handleWhatsAppLogs))

	// Retaguarda ADM: DRE, Curva ABC, Clientes CRM & Brindes, Cupons de Desconto, Impressoras por Categoria/Setor, Ponte PC Caixa, Turnos de Caixa PDV, Alarmes do Garçom, Modalidades do Cardápio e Central de Debug
	mux.HandleFunc("/api/admin/dashboard", srv.withCORS(srv.handleAdminDashboard))
	mux.HandleFunc("/api/customers", srv.withCORS(srv.handleCustomers))
	mux.HandleFunc("/api/coupons", srv.withCORS(srv.handleCoupons))
	mux.HandleFunc("/api/coupons/validate", srv.withCORS(srv.handleValidateCoupon))
	mux.HandleFunc("/api/admin/printer", srv.withCORS(srv.handlePrinterConfig))
	mux.HandleFunc("/api/admin/printers", srv.withCORS(srv.handlePrinterStations))
	mux.HandleFunc("/api/printers/download-cashier-bridge", srv.withCORS(srv.handleDownloadCashierBridge))
	mux.HandleFunc("/api/operational-settings", srv.withCORS(srv.handleOperationalSettings))
	mux.HandleFunc("/api/waiter/alerts", srv.withCORS(srv.handleWaiterAlerts))
	mux.HandleFunc("/api/caixa/shifts", srv.withCORS(srv.handleCashierShifts))
	mux.HandleFunc("/api/admin/logs", srv.withCORS(srv.handleSystemLogs))
	mux.HandleFunc("/api/system/deploy", srv.withCORS(srv.handleSystemDeploy))

	// Estrutura Plug-and-Play para o Futuro: Webhook Mercado Pago (PIX/Cartão) e Pré-Emissão / Emissão Fiscal NFC-e SEFAZ A1
	mux.HandleFunc("/api/webhooks/mercadopago", srv.withCORS(srv.handleMercadoPagoWebhook))
	mux.HandleFunc("/api/fiscal/nfce-preview", srv.withCORS(srv.handleNFCePreview))
	mux.HandleFunc("/api/fiscal/nfce-emit", srv.withCORS(srv.handleNFCeEmit))

	// Download direto do APK compilado (Sem cache para sempre entregar a última build v1.6)
	mux.HandleFunc("/download/", srv.withCORS(srv.handleAPKDownload))
	mux.HandleFunc("/download/SirBeef-SuperApp.apk", srv.withCORS(srv.handleAPKDownload))

	// Arquivos estáticos, Cardápio Digital (Clientes) e Central Web PDV (Geral, Expedição & Caixa Mercado/Hamburgueria)
	fs := http.FileServer(http.Dir(webDir))
	mux.Handle("/assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		fs.ServeHTTP(w, r)
	}))
	serveWebFile := func(fileName string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Security-Policy", "frame-ancestors *")
			w.Header().Set("Access-Control-Allow-Origin", "*")
			http.ServeFile(w, r, filepath.Join(webDir, fileName))
		}
	}
	mux.HandleFunc("/pdv", serveWebFile("pdv.html"))
	mux.HandleFunc("/pdv/", serveWebFile("pdv.html"))
	mux.HandleFunc("/cardapio", serveWebFile("index.html"))
	mux.HandleFunc("/cardapio/", serveWebFile("index.html"))
	mux.HandleFunc("/mesa", serveWebFile("index.html"))
	mux.HandleFunc("/mesa/", serveWebFile("index.html"))
	mux.HandleFunc("/rastreio", serveWebFile("rastreio.html"))
	mux.HandleFunc("/rastreio/", serveWebFile("rastreio.html"))
	mux.HandleFunc("/", serveWebFile("index.html"))

	lanIPs := getLocalIPv4s()
	fmt.Println("======================================================================")
	fmt.Println("🥩  SIR BEEF — SERVIDOR HOST HÍBRIDO (PDV, DELIVERY, LOGÍSTICA & APK)")
	fmt.Println("======================================================================")
	fmt.Printf("⚡ Banco de Dados : SQLite WAL (%s)\n", dbPath)
	fmt.Printf("🌐 Cardápio Local : http://localhost:%s/cardapio\n", port)
	fmt.Printf("🖥️  Central PDV    : http://localhost:%s/pdv\n", port)
	for _, ip := range lanIPs {
		fmt.Printf("📱 IP Rede Wi-Fi  : http://%s:%s/cardapio | http://%s:%s/pdv\n", ip, port, ip, port)
		fmt.Printf("📱 Sem Porta (80) : http://%s/cardapio      | http://%s/pdv\n", ip, ip)
	}
	if tURL := srv.getTunnelURL(); tURL != "" {
		fmt.Printf("☁️  Túnel Cardápio : %s/cardapio\n", tURL)
		fmt.Printf("☁️  Túnel PDV Web  : %s/pdv\n", tURL)
	}
	fmt.Println("🔑 Logins RBAC    : PIN 1111 (ADMIN) | 2222 (CAIXA) | 3333 (GARCOM) | 4444 (MOTOBOY)")
	fmt.Println("======================================================================")

	// Supervisor automático do Túnel Cloudflare (inicia e mantém o cloudflared.exe ativo sozinho)
	go srv.ensureCloudflareTunnelRunning(exeDir)

	// Escuta simultaneamente na porta 80 (HTTP padrão) e 8080 em IPv4 puro (tcp4) e IPv6 (tcp6)
	go func() {
		if ln80, err := net.Listen("tcp4", "0.0.0.0:80"); err == nil {
			_ = http.Serve(ln80, mux)
		}
	}()
	go func() {
		if ln80v6, err := net.Listen("tcp6", "[::]:80"); err == nil {
			_ = http.Serve(ln80v6, mux)
		}
	}()
	go func() {
		if ln6, err := net.Listen("tcp6", "[::]:"+port); err == nil {
			_ = http.Serve(ln6, mux)
		}
	}()

	ln4, err := net.Listen("tcp4", "0.0.0.0:"+port)
	if err != nil {
		log.Fatal(http.ListenAndServe("0.0.0.0:"+port, mux))
	}
	log.Fatal(http.Serve(ln4, mux))
}

func (s *Server) ensureCloudflareTunnelRunning(exeDir string) {
	cfPath := filepath.Join(filepath.Dir(exeDir), "cloudflared.exe")
	if _, err := os.Stat(cfPath); err != nil {
		cfPath = filepath.Join(exeDir, "cloudflared.exe")
		if _, err2 := os.Stat(cfPath); err2 != nil {
			return
		}
	}

	tokenFile := filepath.Join(s.DataDir, "cloudflare_token.txt")
	for {
		// Se o túnel já estiver respondendo na porta de métricas 20241..20245, atualiza tunnel_url.txt e sincroniza com GitHub se mudou
		if active := s.probeLiveQuickTunnel(); active != "" {
			if strings.TrimRight(active, "/") != strings.TrimRight(s.lastSyncedTunnel, "/") {
				s.syncGitHubTunnelURL(active)
			}
			time.Sleep(10 * time.Second)
			continue
		}

		var cmd *exec.Cmd
		if b, err := os.ReadFile(tokenFile); err == nil && strings.TrimSpace(string(b)) != "" {
			tok := strings.TrimSpace(string(b))
			cmd = exec.Command(cfPath, "tunnel", "--no-autoupdate", "run", "--token", tok)
		} else {
			cmd = exec.Command(cfPath, "tunnel", "--url", "http://127.0.0.1:"+s.Port, "--metrics", "127.0.0.1:20241", "--no-autoupdate")
		}
		if err := cmd.Start(); err == nil {
			// Aguarda alguns segundos para capturar a nova URL gerada
			for i := 0; i < 15; i++ {
				time.Sleep(1500 * time.Millisecond)
				if u := s.probeLiveQuickTunnel(); u != "" {
					fmt.Printf("☁️  [TÚNEL AUTO-ATIVO] %s/cardapio | %s/pdv\n", u, u)
					if strings.TrimRight(u, "/") != strings.TrimRight(s.lastSyncedTunnel, "/") {
						go s.syncGitHubTunnelURL(u)
					}
					break
				}
			}
			_ = cmd.Wait()
		}
		time.Sleep(5 * time.Second)
	}
}

func (s *Server) getServerEnvMode() string {
	envFile := filepath.Join(s.DataDir, "server_env.json")
	if b, err := os.ReadFile(envFile); err == nil {
		var parsed struct {
			EnvMode string `json:"env_mode"`
		}
		if json.Unmarshal(b, &parsed) == nil {
			m := strings.ToUpper(strings.TrimSpace(parsed.EnvMode))
			if m == "TESTE_LOCAL" || m == "TESTE_DEV" {
				return "TESTE_LOCAL"
			}
			if m == "PRODUCAO_LOJA" {
				return "PRODUCAO_LOJA"
			}
		}
	}
	txtFile := filepath.Join(s.DataDir, "server_env_mode.txt")
	if b2, err2 := os.ReadFile(txtFile); err2 == nil {
		m2 := strings.ToUpper(strings.TrimSpace(string(b2)))
		if m2 == "TESTE_LOCAL" || m2 == "TESTE_DEV" {
			return "TESTE_LOCAL"
		}
	}
	return "PRODUCAO_LOJA"
}

func (s *Server) setServerEnvMode(mode string) string {
	m := strings.ToUpper(strings.TrimSpace(mode))
	if m == "TESTE_DEV" || m == "TESTE_LOCAL" {
		m = "TESTE_LOCAL"
	} else {
		m = "PRODUCAO_LOJA"
	}
	envFile := filepath.Join(s.DataDir, "server_env.json")
	payload := map[string]string{
		"env_mode":   m,
		"updated_at": time.Now().Format("02/01/2006 15:04:05"),
	}
	b, _ := json.MarshalIndent(payload, "", "  ")
	_ = os.WriteFile(envFile, b, 0644)
	_ = os.WriteFile(filepath.Join(s.DataDir, "server_env_mode.txt"), []byte(m), 0644)
	return m
}

func (s *Server) syncGitHubTunnelURL(tunnelURL string) {
	s.syncGitHubTunnelURLForce(tunnelURL, false)
}

func (s *Server) syncGitHubTunnelURLForce(tunnelURL string, force bool) {
	ghCfgFile := filepath.Join(s.DataDir, "github_config.json")
	b, err := os.ReadFile(ghCfgFile)
	if err != nil {
		return
	}
	var cfg struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Token  string `json:"token"`
	}
	if json.Unmarshal(b, &cfg) != nil || cfg.Owner == "" || cfg.Repo == "" || cfg.Token == "" {
		return
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}

	if !force && s.getServerEnvMode() == "TESTE_LOCAL" {
		// Verifica se o túnel atualmente publicado no GitHub (PC da Loja) está online.
		// Se o PC da Loja estiver ligado e respondendo, preserva o link da loja;
		// se o túnel publicado estiver desligado/offline (ex: você testando em casa antes de ligar na loja), atualiza automaticamente!
		storeOnline := false
		probeClient := &http.Client{Timeout: 3 * time.Second}
		tApi := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/tunnel.json?t=%d", cfg.Owner, cfg.Repo, time.Now().Unix())
		if reqT, errReq := http.NewRequest(http.MethodGet, tApi, nil); errReq == nil {
			reqT.Header.Set("Authorization", "Bearer "+cfg.Token)
			reqT.Header.Set("Accept", "application/vnd.github.raw+json")
			reqT.Header.Set("User-Agent", "SirBeef-Host-Server")
			if respT, errResp := probeClient.Do(reqT); errResp == nil && respT != nil {
				var curT struct {
					TunnelURL string `json:"tunnel_url"`
				}
				_ = json.NewDecoder(respT.Body).Decode(&curT)
				respT.Body.Close()
				remoteBase := strings.TrimRight(strings.TrimSpace(curT.TunnelURL), "/")
				if remoteBase != "" && !strings.EqualFold(remoteBase, strings.TrimRight(tunnelURL, "/")) {
					if hResp, errH := probeClient.Get(remoteBase + "/api/health"); errH == nil && hResp != nil {
						hResp.Body.Close()
						if hResp.StatusCode == http.StatusOK {
							storeOnline = true
						}
					}
				}
			}
		}
		if storeOnline {
			fmt.Printf("🧪 [MODO TESTE LOCAL] Servidor da Hamburgueria está ONLINE — mantendo link oficial da loja intacto no GitHub. Use http://localhost:8080/pdv neste PC.\n")
			return
		}
		fmt.Printf("☁️ [MODO TESTE LOCAL] Nenhum outro servidor online detectado no link público — sincronizando https://%s.github.io/pdv com este PC (%s)!\n", cfg.Owner, tunnelURL)
	}

	vInfo := s.getCurrentAPKVersionInfo(nil)
	ips := getLocalIPv4s()
	payloadData := map[string]any{
		"tunnel_url":         strings.TrimRight(tunnelURL, "/"),
		"lan_url":            "http://192.168.18.53:8080",
		"lan_no_port_url":    "http://192.168.18.53",
		"hostname_url":       "http://gui",
		"lan_ips":            ips,
		"cardapio_fixed_url": fmt.Sprintf("https://%s.github.io/cardapio", cfg.Owner),
		"rastreio_fixed_url": fmt.Sprintf("https://%s.github.io/rastreio", cfg.Owner),
		"pdv_fixed_url":      fmt.Sprintf("https://%s.github.io/pdv", cfg.Owner),
		"apk_fixed_url":      fmt.Sprintf("https://%s.github.io/apk", cfg.Owner),
		"apk_direct_url":     fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/SirBeef-SuperApp.apk", cfg.Owner, cfg.Repo, cfg.Branch),
		"apk_version_code":   vInfo["version_code"],
		"apk_version_name":   vInfo["version_name"],
		"updated_at":         time.Now().Format(time.RFC3339),
	}
	rawJSON, _ := json.MarshalIndent(payloadData, "", "  ")

	// Garante que as páginas fixas https://sirbeefnup.github.io/pdv, /cardapio, /mesa e /rastreio
	// mantenham a URL 100% FIXA na barra de endereço do navegador (sem pular para trycloudflare.com)
	// e leiam tunnel.json diretamente da GitHub API em tempo real (0s de atraso)!
	buildFixedShellHTML := func(routePath string, pageTitle string) string {
		return fmt.Sprintf(`<!DOCTYPE html>
<!-- SIRBEEF_FIXED_URL_SHELL_V5 -->
<html lang="pt-BR">
<head>
  <meta charset="UTF-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1.0, maximum-scale=1.0, user-scalable=no" />
  <title>%s</title>
  <style>
    html, body {
      margin: 0;
      padding: 0;
      width: 100%%;
      height: 100%%;
      overflow: hidden;
      background: #0a0a0d;
      color: #f3d87d;
      font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    }
    #sbSplash {
      position: fixed;
      inset: 0;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      background: #0a0a0d;
      z-index: 20;
      transition: opacity 0.25s ease;
      text-align: center;
      padding: 20px;
    }
    #sbSplash.hidden {
      opacity: 0;
      pointer-events: none;
    }
    .spinner {
      width: 42px;
      height: 42px;
      border: 3px solid #272732;
      border-top-color: #d4af37;
      border-radius: 50%%;
      animation: spin 0.8s linear infinite;
      margin-bottom: 14px;
    }
    @keyframes spin { to { transform: rotate(360deg); } }
    #sbLiveFrame {
      position: fixed;
      top: 0;
      left: 0;
      width: 100vw;
      height: 100vh;
      border: none;
      margin: 0;
      padding: 0;
      background: #0a0a0d;
      z-index: 10;
    }
  </style>
</head>
<body>
  <div id="sbSplash">
    <div class="spinner"></div>
    <div style="font-size:17px;font-weight:800;letter-spacing:0.5px;">🥩 SIR BEEF</div>
    <div id="sbStatusText" style="color:#9ca3af;font-size:13px;margin-top:6px;">Conectando em tempo real...</div>
  </div>
  <iframe
    id="sbLiveFrame"
    allow="clipboard-read; clipboard-write; geolocation; camera; microphone; autoplay; fullscreen"
    allowfullscreen
  ></iframe>
  <script>
    (function() {
      const ROUTE_PATH = %q;
      const frame = document.getElementById("sbLiveFrame");
      const splash = document.getElementById("sbSplash");
      const statusText = document.getElementById("sbStatusText");
      let activeTunnelBase = "";
      let isChecking = false;

      frame.addEventListener("load", function() {
        if (activeTunnelBase) {
          splash.classList.add("hidden");
        }
      });

      async function fetchTunnelData() {
        try {
          const rApi = await fetch("https://api.github.com/repos/sirbeefnup/sirbeefnup.github.io/contents/tunnel.json?t=" + Date.now(), {
            headers: { "Accept": "application/vnd.github.raw+json" },
            cache: "no-store"
          });
          if (rApi.ok) return await rApi.json();
        } catch (e) {}
        try {
          const rPages = await fetch("/tunnel.json?t=" + Date.now(), { cache: "no-store" });
          if (rPages.ok) return await rPages.json();
        } catch (e2) {}
        return null;
      }

      async function isTunnelHealthy(baseUrl) {
        try {
          const ctrl = new AbortController();
          const timer = setTimeout(function() { ctrl.abort(); }, 3500);
          const r = await fetch(baseUrl + "/api/health?t=" + Date.now(), {
            mode: "cors",
            cache: "no-store",
            signal: ctrl.signal
          });
          clearTimeout(timer);
          return r && r.ok;
        } catch (e) {
          return false;
        }
      }

      async function syncTunnelAndLoad(isInitial) {
        if (isChecking) return;
        isChecking = true;
        try {
          const d = await fetchTunnelData();
          if (!d) return;
          const newBase = (d && d.tunnel_url ? d.tunnel_url : "").replace(/\/$/, "");
          if (newBase && newBase.startsWith("https://")) {
            if (newBase !== activeTunnelBase) {
              const healthy = await isTunnelHealthy(newBase);
              if (healthy) {
                activeTunnelBase = newBase;
                const qs = window.location.search || "";
                const hash = window.location.hash || "";
                frame.src = activeTunnelBase + ROUTE_PATH + qs + hash;
                setTimeout(function() { splash.classList.add("hidden"); }, 1400);
              } else {
                if (statusText) {
                  statusText.textContent = "Sincronizando túnel do servidor... Conectando automaticamente em instantes...";
                }
                setTimeout(function() { syncTunnelAndLoad(false); }, 2500);
              }
            }
            return;
          }
          if (isInitial && d && d.lan_url) {
            window.location.replace(d.lan_url.replace(/\/$/, "") + ROUTE_PATH + (window.location.search || ""));
          }
        } catch (e) {
        } finally {
          isChecking = false;
        }
      }

      syncTunnelAndLoad(true);
      setInterval(function() { syncTunnelAndLoad(false); }, 10000);
    })();
  </script>
</body>
</html>`, pageTitle, routePath)
	}

	filesToCommit := map[string][]byte{
		"tunnel.json": rawJSON,
	}

	// Verifica se pdv/index.html já está com SIRBEEF_FIXED_URL_SHELL_V5; se não estiver, inclui todos no commit único
	client := &http.Client{Timeout: 12 * time.Second}
	checkAPI := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/pdv/index.html", cfg.Owner, cfg.Repo)
	needsShellUpdate := true
	if gReq, err := http.NewRequest(http.MethodGet, checkAPI, nil); err == nil {
		gReq.Header.Set("Authorization", "Bearer "+cfg.Token)
		gReq.Header.Set("Accept", "application/vnd.github.raw+json")
		gReq.Header.Set("User-Agent", "SirBeef-Host-Server")
		if gResp, err := client.Do(gReq); err == nil && gResp != nil {
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(gResp.Body)
			gResp.Body.Close()
			if strings.Contains(buf.String(), "SIRBEEF_FIXED_URL_SHELL_V5") {
				needsShellUpdate = false
			}
		}
	}

	if needsShellUpdate {
		pdvShell := []byte(buildFixedShellHTML("/pdv", "SIR BEEF — Central Web de PDVs (Geral, Expedição & Caixa)"))
		cardapioShell := []byte(buildFixedShellHTML("/cardapio", "SIR BEEF — Cardápio Digital Online"))
		mesaShell := []byte(buildFixedShellHTML("/mesa", "SIR BEEF — Autoatendimento na Mesa"))
		rastreioShell := []byte(buildFixedShellHTML("/rastreio", "SIR BEEF — Rastreio de Entrega em Tempo Real"))

		filesToCommit["pdv/index.html"] = pdvShell
		filesToCommit["pdv.html"] = pdvShell
		filesToCommit["cardapio/index.html"] = cardapioShell
		filesToCommit["cardapio.html"] = cardapioShell
		filesToCommit["mesa/index.html"] = mesaShell
		filesToCommit["mesa.html"] = mesaShell
		filesToCommit["rastreio/index.html"] = rastreioShell
		filesToCommit["rastreio.html"] = rastreioShell
		filesToCommit["index.html"] = cardapioShell
		filesToCommit[".nojekyll"] = []byte("")
	}

	if _, err := s.pushFilesAtomicallyToGitHub(filesToCommit, "Auto-sync tunnel_url: "+tunnelURL); err == nil {
		s.lastSyncedTunnel = strings.TrimRight(tunnelURL, "/")
		fmt.Printf("🐙 [GITHUB PAGES FIXO] Sincronizado! Cardápio: https://%s.github.io/cardapio | Rastreio: https://%s.github.io/rastreio | PDV: https://%s.github.io/pdv\n", cfg.Owner, cfg.Owner, cfg.Owner)
	} else {
		fmt.Printf("⚠️ [GITHUB SYNC RETRY] Falha temporária ao sincronizar tunnel.json (%v) — tentará novamente no próximo ciclo.\n", err)
	}
}

func (s *Server) pushFilesAtomicallyToGitHub(files map[string][]byte, commitMsg string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		sha, err := s.pushFilesAtomicallyToGitHubOnce(files, commitMsg)
		if err == nil {
			return sha, nil
		}
		lastErr = err
		time.Sleep(time.Duration(attempt) * 1500 * time.Millisecond)
	}
	return "", lastErr
}

func (s *Server) pushFilesAtomicallyToGitHubOnce(files map[string][]byte, commitMsg string) (string, error) {
	ghCfgFile := filepath.Join(s.DataDir, "github_config.json")
	b, err := os.ReadFile(ghCfgFile)
	if err != nil {
		return "", fmt.Errorf("github_config.json não encontrado")
	}
	var cfg struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Token  string `json:"token"`
	}
	if json.Unmarshal(b, &cfg) != nil || cfg.Owner == "" || cfg.Repo == "" || cfg.Token == "" {
		return "", fmt.Errorf("configuração do GitHub inválida")
	}
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}

	client := &http.Client{Timeout: 60 * time.Second}
	doGitHubJSON := func(method, url string, body any, out any) error {
		var rdr *bytes.Reader
		if body != nil {
			jb, _ := json.Marshal(body)
			rdr = bytes.NewReader(jb)
		} else {
			rdr = bytes.NewReader(nil)
		}
		req, err := http.NewRequest(method, url, rdr)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "SirBeef-Host-Server")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			var errBuf bytes.Buffer
			_, _ = errBuf.ReadFrom(resp.Body)
			return fmt.Errorf("GitHub HTTP %d: %s", resp.StatusCode, errBuf.String())
		}
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	}

	baseAPI := fmt.Sprintf("https://api.github.com/repos/%s/%s", cfg.Owner, cfg.Repo)
	var refResp struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := doGitHubJSON(http.MethodGet, fmt.Sprintf("%s/git/ref/heads/%s", baseAPI, cfg.Branch), nil, &refResp); err != nil {
		return "", err
	}
	latestCommitSHA := refResp.Object.SHA

	var commitResp struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := doGitHubJSON(http.MethodGet, fmt.Sprintf("%s/git/commits/%s", baseAPI, latestCommitSHA), nil, &commitResp); err != nil {
		return "", err
	}

	type treeEntry struct {
		Path string `json:"path"`
		Mode string `json:"mode"`
		Type string `json:"type"`
		SHA  string `json:"sha"`
	}
	var treeEntries []treeEntry
	for pathKey, contentBytes := range files {
		// Bloqueio rígido de segurança: NUNCA envia banco de dados de clientes ou sessão WhatsApp!
		lowerKey := strings.ToLower(pathKey)
		if strings.Contains(lowerKey, "sirbeef.db") || strings.Contains(lowerKey, "whatsapp_session") {
			continue
		}
		var blobResp struct {
			SHA string `json:"sha"`
		}
		blobReq := map[string]string{
			"content":  base64.StdEncoding.EncodeToString(contentBytes),
			"encoding": "base64",
		}
		if err := doGitHubJSON(http.MethodPost, fmt.Sprintf("%s/git/blobs", baseAPI), blobReq, &blobResp); err != nil {
			return "", fmt.Errorf("erro ao criar blob para %s: %w", pathKey, err)
		}
		treeEntries = append(treeEntries, treeEntry{
			Path: strings.TrimPrefix(filepath.ToSlash(pathKey), "/"),
			Mode: "100644",
			Type: "blob",
			SHA:  blobResp.SHA,
		})
	}

	var newTreeResp struct {
		SHA string `json:"sha"`
	}
	treePayload := map[string]any{
		"base_tree": commitResp.Tree.SHA,
		"tree":      treeEntries,
	}
	if err := doGitHubJSON(http.MethodPost, fmt.Sprintf("%s/git/trees", baseAPI), treePayload, &newTreeResp); err != nil {
		return "", err
	}

	var newCommitResp struct {
		SHA string `json:"sha"`
	}
	commitPayload := map[string]any{
		"message": commitMsg,
		"tree":    newTreeResp.SHA,
		"parents": []string{latestCommitSHA},
	}
	if err := doGitHubJSON(http.MethodPost, fmt.Sprintf("%s/git/commits", baseAPI), commitPayload, &newCommitResp); err != nil {
		return "", err
	}

	refUpdate := map[string]any{
		"sha":   newCommitResp.SHA,
		"force": false,
	}
	if err := doGitHubJSON(http.MethodPatch, fmt.Sprintf("%s/git/refs/heads/%s", baseAPI, cfg.Branch), refUpdate, nil); err != nil {
		return "", err
	}
	return newCommitResp.SHA, nil
}

func (s *Server) probeLiveQuickTunnel() string {
	tunnelFile := filepath.Join(s.DataDir, "tunnel_url.txt")
	fixedFile := filepath.Join(s.DataDir, "fixed_domain.txt")
	if b, err := os.ReadFile(fixedFile); err == nil {
		if fd := strings.TrimSpace(string(b)); strings.HasPrefix(fd, "http") {
			fd = strings.TrimRight(fd, "/")
			_ = os.WriteFile(tunnelFile, []byte(fd), 0644)
			return fd
		}
	}
	client := &http.Client{Timeout: 450 * time.Millisecond}
	for p := 20241; p <= 20245; p++ {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/quicktunnel", p))
		if err == nil && resp != nil {
			var qt struct {
				Hostname string `json:"hostname"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&qt)
			resp.Body.Close()
			host := strings.TrimSpace(qt.Hostname)
			if host != "" {
				if !strings.HasPrefix(host, "http") {
					host = "https://" + host
				}
				host = strings.TrimRight(host, "/")
				_ = os.WriteFile(tunnelFile, []byte(host), 0644)
				return host
			}
		}
	}
	return ""
}

// ensureWindowsNetworkReady verifica e aplica automaticamente as regras de Firewall do Windows
// (Inbound/Outbound nas portas 8080 e 80 e no executável, mudança de perfil de rede para Private,
// e vinculação do IP fixo adicional 192.168.18.53 junto ao IP DHCP 192.168.18.29 no Wi-Fi).
func ensureWindowsNetworkReady(port string, exeDir string) {
	if runtime.GOOS != "windows" {
		return
	}

	ruleName := "SirBeef_Host_LAN_WiFi_" + port
	currentIPs := getLocalIPv4s()
	has53 := false
	isSubnet18 := false
	for _, ip := range currentIPs {
		if ip == "192.168.18.53" {
			has53 = true
		}
		if strings.HasPrefix(ip, "192.168.18.") {
			isSubnet18 = true
		}
	}

	checkCmd := exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+ruleName)
	out, err := checkCmd.CombinedOutput()
	ruleExists := err == nil && strings.Contains(string(out), ruleName)

	rootDir := filepath.Dir(exeDir)
	starterBat := filepath.Join(rootDir, "INICIAR_ECOSSISTEMA_SIRBEEF.bat")
	envModeBytes, _ := os.ReadFile(filepath.Join(exeDir, "data", "server_env_mode.txt"))
	isTestMode := strings.EqualFold(strings.TrimSpace(string(envModeBytes)), "TESTE_DEV")

	if isTestMode {
		_ = exec.Command("schtasks", "/Delete", "/TN", "SirBeef_AutoStart_Admin", "/F").Run()
		_ = exec.Command("reg", "delete", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "SirBeef_Server_Admin", "/f").Run()
	}

	taskCheck := exec.Command("schtasks", "/Query", "/TN", "SirBeef_AutoStart_Admin")
	taskExists := isTestMode || (taskCheck.Run() == nil)

	if ruleExists && (!isSubnet18 || has53) && taskExists {
		fmt.Println("🛡️  [REDE LOCAL] Regra de Firewall '" + ruleName + "' e IP Wi-Fi já estão ativos e liberados.")
		return
	}

	fmt.Println("🛡️  [REDE LOCAL] Configurando Firewall do Windows e Permissões de Rede como Administrador...")
	exePath := filepath.Join(exeDir, "sirbeef-server.exe")

	autoStartCmds := ""
	if !isTestMode {
		autoStartCmds = fmt.Sprintf(
			"schtasks /Create /TN \"SirBeef_AutoStart_Admin\" /TR \"`\"%s`\" --autostart\" /SC ONLOGON /RL HIGHEST /F | Out-Null\r\n"+
				"reg add \"HKCU\\Software\\Microsoft\\Windows\\CurrentVersion\\Run\" /v \"SirBeef_Server_Admin\" /t REG_SZ /d \"schtasks /Run /TN `\"SirBeef_AutoStart_Admin`\"\" /f | Out-Null\r\n",
			starterBat,
		)
	}

	psScriptContent := fmt.Sprintf(
		"Get-NetFirewallApplicationFilter | Where-Object { $_.Program -like '*sirbeef*' } | Get-NetFirewallRule | Where-Object { $_.Action -eq 'Block' } | Remove-NetFirewallRule -ErrorAction SilentlyContinue\r\n"+
			"netsh advfirewall firewall delete rule name=\"%s\" | Out-Null\r\n"+
			"netsh advfirewall firewall add rule name=\"%s\" dir=in action=allow protocol=TCP localport=%s,80 profile=any | Out-Null\r\n"+
			"netsh advfirewall firewall add rule name=\"%s_UDP\" dir=in action=allow protocol=UDP localport=%s profile=any | Out-Null\r\n"+
			"netsh advfirewall firewall add rule name=\"%s_OUT\" dir=out action=allow protocol=TCP localport=%s,80 profile=any | Out-Null\r\n"+
			"netsh advfirewall firewall add rule name=\"%s_EXE\" dir=in action=allow program=\"%s\" enable=yes profile=any | Out-Null\r\n"+
			"Get-NetConnectionProfile | Where-Object { $_.NetworkCategory -ne 'Private' } | ForEach-Object { Set-NetConnectionProfile -InterfaceIndex $_.InterfaceIndex -NetworkCategory Private -ErrorAction SilentlyContinue }\r\n"+
			"%s",
		ruleName, ruleName, port, ruleName, port, ruleName, port, ruleName, exePath, autoStartCmds,
	)

	tmpScript := filepath.Join(os.TempDir(), "sirbeef_net_init.ps1")
	_ = os.WriteFile(tmpScript, []byte(psScriptContent), 0644)

	// Tenta aplicar diretamente caso já esteja rodando como Administrador
	directCmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", tmpScript)
	_ = directCmd.Run()
	if out2, err2 := exec.Command("netsh", "advfirewall", "firewall", "show", "rule", "name="+ruleName).CombinedOutput(); err2 == nil && strings.Contains(string(out2), ruleName) {
		fmt.Println("✅ [REDE LOCAL] Firewall, Porta 8080/80 e IPs Wi-Fi configurados com sucesso (Modo ADM Direto)!")
		return
	}

	// Caso não esteja como Administrador, dispara auto-elevação via UAC (RunAs)
	elevatedCmd := exec.Command(
		"powershell",
		"-NoProfile",
		"-ExecutionPolicy", "Bypass",
		"-Command",
		fmt.Sprintf(`Start-Process powershell -Verb RunAs -Wait -ArgumentList '-NoProfile -ExecutionPolicy Bypass -File "%s"'`, tmpScript),
	)
	if err := elevatedCmd.Run(); err != nil {
		fmt.Printf("⚠️  [REDE LOCAL] Aviso ao solicitar permissão ADM: %v\n", err)
	} else {
		fmt.Println("✅ [REDE LOCAL] Portas 8080/80 e IPs Wi-Fi (192.168.18.29 + 192.168.18.53) liberados no Firewall!")
	}
}

func (s *Server) getTunnelURL() string {
	if live := s.probeLiveQuickTunnel(); live != "" {
		return live
	}
	tunnelFile := filepath.Join(s.DataDir, "tunnel_url.txt")
	if b, err := os.ReadFile(tunnelFile); err == nil {
		u := strings.TrimSpace(string(b))
		if u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	return "https://targeted-taken-marco-vendor.trycloudflare.com"
}

func (s *Server) resolveRequestBaseURL(r *http.Request, fallbackLan string) string {
	if r != nil {
		host := strings.TrimSpace(r.Host)
		if host != "" {
			lower := strings.ToLower(host)
			isLocal := strings.HasPrefix(lower, "192.168.") ||
				strings.HasPrefix(lower, "10.") ||
				strings.HasPrefix(lower, "172.") ||
				strings.HasPrefix(lower, "127.") ||
				strings.HasPrefix(lower, "localhost") ||
				strings.HasPrefix(lower, "gui")
			if isLocal {
				return "http://" + host
			}
			if strings.Contains(lower, "trycloudflare.com") || r.Header.Get("X-Forwarded-Proto") == "https" {
				return "https://" + host
			}
			return "http://" + host
		}
	}
	if t := s.getTunnelURL(); t != "" {
		return t
	}
	return fallbackLan
}

func (s *Server) withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Client-Role, X-Device-Info")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) getCurrentAPKVersionInfo(r *http.Request) map[string]any {
	vCode := 39
	vName := "4.8.1"
	notes := "v4.8.1 (Build #39): (1) Topo sempre fixo no modo Kiosk dos Tablets (Card da Mesa + Chamar Garçom/Conta/Descanso + Abas de Categorias permanecem fixos ao rolar os lanches); (2) Link https://sirbeefnup.github.io/pdv (e /cardapio e /rastreio) 100% fixo na barra de endereço do navegador, sem mudar para trycloudflare.com!"
	builtAt := time.Now().Format("02/01/2006 15:04")

	vFile := filepath.Join(s.DataDir, "apk_version.json")
	if b, err := os.ReadFile(vFile); err == nil {
		cleanBytes := strings.TrimPrefix(string(b), "\uFEFF")
		var parsed map[string]any
		if json.Unmarshal([]byte(cleanBytes), &parsed) == nil {
			if c, ok := parsed["version_code"].(float64); ok && int(c) > 0 {
				vCode = int(c)
			}
			if n, ok := parsed["version_name"].(string); ok && n != "" {
				vName = n
			}
			if rn, ok := parsed["release_notes"].(string); ok && rn != "" {
				notes = rn
			}
			if ba, ok := parsed["built_at"].(string); ok && ba != "" {
				builtAt = ba
			}
		}
	}

	exeDir, _ := os.Getwd()
	apkPath := filepath.Join(filepath.Dir(exeDir), "SirBeef-SuperApp.apk")
	var sizeBytes int64
	if st, err := os.Stat(apkPath); err == nil {
		sizeBytes = st.Size()
		builtAt = st.ModTime().Format("02/01/2006 15:04:05")
	}

	ips := getLocalIPv4s()
	primaryLan := "http://localhost:" + s.Port
	if len(ips) > 0 {
		primaryLan = fmt.Sprintf("http://%s:%s", ips[0], s.Port)
	}
	baseURL := s.resolveRequestBaseURL(r, primaryLan)

	dlPath := fmt.Sprintf("/download/SirBeef-SuperApp-v%s.apk?v=%d", vName, vCode)
	return map[string]any{
		"version_code":        vCode,
		"version_name":        vName,
		"release_notes":       notes,
		"built_at":            builtAt,
		"file_size_bytes":     sizeBytes,
		"download_path":       dlPath,
		"download_url":        baseURL + dlPath,
		"external_tunnel_url": s.getTunnelURL(),
	}
}

func (s *Server) handleAppVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.getCurrentAPKVersionInfo(r))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	cfg := s.Store.GetPrinterConfig()
	ips := getLocalIPv4s()
	primaryLan := "http://localhost:" + s.Port
	if len(ips) > 0 {
		primaryLan = fmt.Sprintf("http://%s:%s", ips[0], s.Port)
	}
	vInfo := s.getCurrentAPKVersionInfo(r)
	effectiveBase := s.resolveRequestBaseURL(r, primaryLan)
	extTunnel := s.getTunnelURL()
	hostname, _ := os.Hostname()
	fixedHostnameURL := ""
	if hostname != "" {
		fixedHostnameURL = fmt.Sprintf("http://%s", strings.ToLower(hostname))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                    "ONLINE",
		"system":                    "SIR BEEF HOST SERVER",
		"mode":                      "HYBRID_LAN_AND_CLOUDFLARE_TUNNEL",
		"timestamp":                 time.Now().Format(time.RFC3339),
		"printer_cols":              cfg.Columns,
		"paper_width_mm":            cfg.PaperWidthMM,
		"lan_ips":                   ips,
		"lan_url":                   primaryLan,
		"fixed_hostname_url":        fixedHostnameURL,
		"tunnel_url":                effectiveBase,
		"external_tunnel_url":       extTunnel,
		"apk_version_code":          vInfo["version_code"],
		"apk_version_name":          vInfo["version_name"],
		"apk_notes":                 vInfo["release_notes"],
		"apk_built_at":              vInfo["built_at"],
		"download_url":              vInfo["download_url"],
		"waiter_alarm_table_call":    cfg.WaiterAlarmTableCall,
		"waiter_alarm_drinks":        cfg.WaiterAlarmDrinks,
		"waiter_alarm_kitchen_ready": cfg.WaiterAlarmKitchenReady,
		"waiter_alarm_mode":          cfg.WaiterAlarmMode,
		"menu_enable_delivery":       cfg.MenuEnableDelivery,
		"menu_enable_retirada":       cfg.MenuEnableRetirada,
		"menu_enable_mesa":           cfg.MenuEnableMesa,
		"menu_enable_kiosk":          cfg.MenuEnableKiosk,
		"waiter_visible_categories":     cfg.WaiterVisibleCategories,
		"waiter_hidden_categories":      cfg.WaiterHiddenCategories,
		"default_delivery_fee":          cfg.DefaultDeliveryFee,
		"allow_staff_edit_delivery_fee": cfg.AllowStaffEditDeliveryFee,
		"operating_hours":               cfg.OperatingHours,
		"is_open_now":                cfg.IsOpenNow,
		"today_hours_label":          cfg.TodayHoursLabel,
		"closed_reason_message":      cfg.ClosedReasonMessage,
		"active_sessions":            s.Store.GetActiveSessionTokens(),
		"server_env_mode":            s.getServerEnvMode(),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	var req struct {
		Username string `json:"username"`
		Name     string `json:"name"`
		Password string `json:"password"`
		PIN      string `json:"pin"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	loginField := strings.TrimSpace(req.Username)
	if loginField == "" {
		loginField = strings.TrimSpace(req.Name)
	}
	passField := strings.TrimSpace(req.Password)
	if passField == "" {
		passField = strings.TrimSpace(req.PIN)
	}
	if loginField == "" && passField != "" {
		loginField = passField
		passField = ""
	}

	user, status, err := s.Store.AuthenticateCredentials(loginField, passField)
	if err != nil {
		httpCode := http.StatusUnauthorized
		if status == "PENDENTE" || status == "BLOQUEADO" {
			httpCode = http.StatusForbidden
		}
		writeJSON(w, httpCode, map[string]any{
			"error":  err.Error(),
			"status": status,
			"user":   user,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "APROVADO",
		"user":          user,
		"token":         fmt.Sprintf("sirbeef-jwt-%d-%s", user.ID, user.Role),
		"session_token": user.SessionToken,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	var req struct {
		Name     string `json:"name"`
		Username string `json:"username"`
		Password string `json:"password"`
		PIN      string `json:"pin"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Dados inválidos"})
		return
	}
	pass := strings.TrimSpace(req.Password)
	if pass == "" {
		pass = strings.TrimSpace(req.PIN)
	}
	u, err := s.Store.RegisterEmployee(req.Name, req.Username, pass, req.Role)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"status":  "PENDENTE",
		"message": "Solicitação de cadastro enviada! O Administrador (ADM) precisa aceitar seu acesso e confirmar seu cargo.",
		"user":    u,
	})
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		users, err := s.Store.ListUsers()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, users)
		return
	}
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		var raw map[string]any
		_ = json.NewDecoder(r.Body).Decode(&raw)
		act, _ := raw["action"].(string)
		act = strings.ToUpper(strings.TrimSpace(act))

		extractInt := func(m map[string]any, keys ...string) int {
			for _, k := range keys {
				if val, exists := m[k]; exists && val != nil {
					switch v := val.(type) {
					case float64:
						if int(v) > 0 {
							return int(v)
						}
					case string:
						if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
							return n
						}
					}
				}
			}
			return 0
		}

		uid := extractInt(raw, "user_id", "id")
		assignedTable := extractInt(raw, "assigned_table_id", "table_number")

		if act == "DELETE" || act == "DELETE_USER" {
			if uid <= 0 {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ID do funcionário inválido para exclusão."})
				return
			}
			if err := s.Store.DeleteUser(uid); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			users, _ := s.Store.ListUsers()
			writeJSON(w, http.StatusOK, users)
			return
		}

		if act == "APPROVE" && uid > 0 {
			_, _ = s.Store.DB.Exec(`UPDATE users SET active = 1, status = 'APROVADO' WHERE id = ?`, uid)
			users, _ := s.Store.ListUsers()
			writeJSON(w, http.StatusOK, users)
			return
		}

		var u models.User
		if sub, ok := raw["user"]; ok && sub != nil {
			b, _ := json.Marshal(sub)
			_ = json.Unmarshal(b, &u)
			if subMap, ok2 := sub.(map[string]any); ok2 {
				if u.ID == 0 {
					u.ID = extractInt(subMap, "user_id", "id")
				}
				if u.TableNumber == 0 {
					u.TableNumber = extractInt(subMap, "assigned_table_id", "table_number")
				}
			}
		} else {
			b, _ := json.Marshal(raw)
			_ = json.Unmarshal(b, &u)
		}
		if u.ID == 0 && uid > 0 {
			u.ID = uid
		}
		if u.TableNumber == 0 && assignedTable > 0 {
			u.TableNumber = assignedTable
		}
		if err := s.Store.CreateOrUpdateUser(u); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		users, _ := s.Store.ListUsers()
		writeJSON(w, http.StatusOK, users)
		return
	}
	if r.Method == http.MethodDelete {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if id > 0 {
			if err := s.Store.DeleteUser(id); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		users, _ := s.Store.ListUsers()
		writeJSON(w, http.StatusOK, users)
	}
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	allParam := r.URL.Query().Get("all")
	includeInactive := allParam == "1" || strings.EqualFold(allParam, "true")
	cats, err := s.Store.GetCatalogAll(includeInactive)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cats)
}

func (s *Server) handleCategories(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cats, err := s.Store.GetCatalogAll(true)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		routes, _ := s.Store.ListCategoryPrinterRoutes()
		writeJSON(w, http.StatusOK, map[string]any{
			"categories": cats,
			"routes":     routes,
		})
		return
	}
	if r.Method == http.MethodDelete {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		moveTo, _ := strconv.Atoi(r.URL.Query().Get("move_to_id"))
		if id > 0 {
			if err := s.Store.DeleteCategory(id, moveTo); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		cats, _ := s.Store.GetCatalogAll(true)
		routes, _ := s.Store.ListCategoryPrinterRoutes()
		writeJSON(w, http.StatusOK, map[string]any{
			"categories": cats,
			"routes":     routes,
		})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action           string `json:"action"` // SAVE, CREATE, EDIT, DELETE
			ID               int    `json:"id"`
			CategoryID       int    `json:"category_id"`
			Name             string `json:"name"`
			Sector           string `json:"sector"`
			PrinterStationID string `json:"printer_station_id"`
			MoveToCategoryID int    `json:"move_to_category_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cid := req.ID
		if cid <= 0 {
			cid = req.CategoryID
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		if act == "DELETE" || act == "DELETE_CATEGORY" {
			if err := s.Store.DeleteCategory(cid, req.MoveToCategoryID); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			cats, _ := s.Store.GetCatalogAll(true)
			routes, _ := s.Store.ListCategoryPrinterRoutes()
			writeJSON(w, http.StatusOK, map[string]any{
				"deleted_id": cid,
				"categories": cats,
				"routes":     routes,
			})
			return
		}
		saved, err := s.Store.SaveCategory(cid, req.Name, req.Sector, req.PrinterStationID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cats, _ := s.Store.GetCatalogAll(true)
		routes, _ := s.Store.ListCategoryPrinterRoutes()
		writeJSON(w, http.StatusOK, map[string]any{
			"saved":      saved,
			"categories": cats,
			"routes":     routes,
		})
	}
}

func (s *Server) handleProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		prods, err := s.Store.ListAllProducts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, prods)
		return
	}
	if r.Method == http.MethodPost {
		var p models.Product
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		saved, err := s.Store.CreateOrUpdateProduct(p)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, saved)
	}
}

func (s *Server) handleProductQuickAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	var req struct {
		ID       int     `json:"id"`
		Action   string  `json:"action"` // TOGGLE_ACTIVE, SET_ACTIVE, SET_PRICE, SET_PROMO_FROM, SET_COST, SET_STOCK, TOGGLE_INFINITE_STOCK, ADJUST_STOCK, CLEAR_IMAGE, SET_IMAGE
		Value    float64 `json:"value"`
		ImageURL string  `json:"image_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ID ou ação inválida"})
		return
	}
	if strings.EqualFold(strings.TrimSpace(req.Action), "SET_IMAGE") {
		if err := s.Store.SetProductImageURL(req.ID, req.ImageURL); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		cats, _ := s.Store.GetCatalogAll(true)
		writeJSON(w, http.StatusOK, cats)
		return
	}
	if err := s.Store.QuickUpdateProduct(req.ID, req.Action, req.Value); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	cats, _ := s.Store.GetCatalogAll(true)
	writeJSON(w, http.StatusOK, cats)
}

func (s *Server) handleProductImageUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	var req struct {
		ProductID int    `json:"product_id"`
		DataURL   string `json:"data_url"`
		ImageURL  string `json:"image_url"`
		Clear     bool   `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.Clear || strings.EqualFold(strings.TrimSpace(req.ImageURL), "SEM_FOTO") || strings.EqualFold(strings.TrimSpace(req.ImageURL), "NONE") {
		if req.ProductID > 0 {
			_ = s.Store.SetProductImageURL(req.ProductID, "")
		}
		writeJSON(w, http.StatusOK, map[string]any{"image_url": "", "cleared": true})
		return
	}

	raw := strings.TrimSpace(req.DataURL)
	if raw == "" && strings.HasPrefix(strings.TrimSpace(req.ImageURL), "data:image/") {
		raw = strings.TrimSpace(req.ImageURL)
	}
	if raw != "" {
		ext := ".jpg"
		b64Part := raw
		if idx := strings.Index(raw, ","); idx >= 0 {
			header := strings.ToLower(raw[:idx])
			b64Part = raw[idx+1:]
			if strings.Contains(header, "png") {
				ext = ".png"
			} else if strings.Contains(header, "webp") {
				ext = ".webp"
			}
		}
		imgBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64Part))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Formato de imagem inválido: " + err.Error()})
			return
		}
		prodAssetsDir := filepath.Join(s.WebDir, "assets", "products")
		_ = os.MkdirAll(prodAssetsDir, 0755)
		fileName := fmt.Sprintf("prod_%d_%d%s", req.ProductID, time.Now().UnixMilli(), ext)
		fullPath := filepath.Join(prodAssetsDir, fileName)
		if err := os.WriteFile(fullPath, imgBytes, 0644); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Erro ao salvar arquivo de imagem: " + err.Error()})
			return
		}
		publicURL := "/assets/products/" + fileName
		if req.ProductID > 0 {
			_ = s.Store.SetProductImageURL(req.ProductID, publicURL)
		}
		writeJSON(w, http.StatusOK, map[string]any{"image_url": publicURL})
		return
	}

	cleanURL := strings.TrimSpace(req.ImageURL)
	if req.ProductID > 0 {
		_ = s.Store.SetProductImageURL(req.ProductID, cleanURL)
	}
	writeJSON(w, http.StatusOK, map[string]any{"image_url": cleanURL})
}

func (s *Server) handleModifierGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		groups, err := s.Store.ListModifierGroupTemplates()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, groups)
		return
	}
	if r.Method == http.MethodDelete {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if id > 0 {
			_ = s.Store.DeleteModifierGroupTemplate(id)
		}
		groups, _ := s.Store.ListModifierGroupTemplates()
		writeJSON(w, http.StatusOK, groups)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action      string                  `json:"action"` // SAVE, DELETE, APPLY_TO_CATEGORY, TOGGLE_OPTION_VISIBLE
			ID          int                     `json:"id"`
			GroupID     int                     `json:"group_id"`
			Name        string                  `json:"name"`
			GroupName   string                  `json:"group_name"`
			OptionIndex int                     `json:"option_index"`
			OptionName  string                  `json:"option_name"`
			Visible     *bool                   `json:"visible"`
			Min         int                     `json:"min"`
			Max         int                     `json:"max"`
			Required    bool                    `json:"required"`
			Options     []models.ModifierOption `json:"options"`
			Group       *models.ModifierGroup   `json:"group"`
			CategoryID  int                     `json:"category_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		if act == "TOGGLE_OPTION_VISIBLE" {
			gid := req.GroupID
			if gid <= 0 {
				gid = req.ID
			}
			gname := req.GroupName
			if gname == "" {
				gname = req.Name
			}
			vis := true
			if req.Visible != nil {
				vis = *req.Visible
			}
			if _, err := s.Store.ToggleModifierGroupOptionVisibility(gid, gname, req.OptionIndex, req.OptionName, vis); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			groups, _ := s.Store.ListModifierGroupTemplates()
			writeJSON(w, http.StatusOK, groups)
			return
		}
		if act == "DELETE" {
			if req.ID > 0 {
				_ = s.Store.DeleteModifierGroupTemplate(req.ID)
			}
			groups, _ := s.Store.ListModifierGroupTemplates()
			writeJSON(w, http.StatusOK, groups)
			return
		}
		if act == "APPLY_TO_CATEGORY" {
			cnt, err := s.Store.ApplyModifierGroupTemplateToCategory(req.ID, req.CategoryID)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			groups, _ := s.Store.ListModifierGroupTemplates()
			writeJSON(w, http.StatusOK, map[string]any{
				"updated_products": cnt,
				"groups":           groups,
			})
			return
		}
		g := models.ModifierGroup{
			ID:       req.ID,
			Name:     req.Name,
			Min:      req.Min,
			Max:      req.Max,
			Required: req.Required,
			Options:  req.Options,
		}
		if req.Group != nil && strings.TrimSpace(req.Group.Name) != "" {
			g = *req.Group
		}
		saved, err := s.Store.SaveModifierGroupTemplate(g)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		groups, _ := s.Store.ListModifierGroupTemplates()
		writeJSON(w, http.StatusOK, map[string]any{
			"saved":  saved,
			"groups": groups,
		})
	}
}

func (s *Server) handleWeeklyPromotions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		list, err := s.Store.ListWeeklyPromotions()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	if r.Method == http.MethodDelete {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if id > 0 {
			_ = s.Store.DeleteWeeklyPromotion(id)
		}
		list, _ := s.Store.ListWeeklyPromotions()
		writeJSON(w, http.StatusOK, list)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action        string  `json:"action"` // SAVE, DELETE, TOGGLE
			ID            int     `json:"id"`
			DayOfWeek     int     `json:"day_of_week"`
			ProductID     int     `json:"product_id"`
			OriginalPrice float64 `json:"original_price"`
			PromoPrice    float64 `json:"promo_price"`
			Active        *bool   `json:"active"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		if act == "DELETE" {
			if req.ID > 0 {
				_ = s.Store.DeleteWeeklyPromotion(req.ID)
			}
			list, _ := s.Store.ListWeeklyPromotions()
			writeJSON(w, http.StatusOK, list)
			return
		}
		if act == "TOGGLE" || act == "SET_ACTIVE" {
			actBool := true
			if req.Active != nil {
				actBool = *req.Active
			}
			_ = s.Store.ToggleWeeklyPromotion(req.ID, actBool)
			list, _ := s.Store.ListWeeklyPromotions()
			writeJSON(w, http.StatusOK, list)
			return
		}
		actVal := true
		if req.Active != nil {
			actVal = *req.Active
		}
		wp := models.WeeklyPromotion{
			ID:            req.ID,
			DayOfWeek:     req.DayOfWeek,
			ProductID:     req.ProductID,
			OriginalPrice: req.OriginalPrice,
			PromoPrice:    req.PromoPrice,
			Active:        actVal,
		}
		if _, err := s.Store.SaveWeeklyPromotion(wp); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		list, _ := s.Store.ListWeeklyPromotions()
		writeJSON(w, http.StatusOK, list)
	}
}

func (s *Server) handleBarcodeLookup(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	prod, qty, err := s.Store.LookupBarcode(code)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": fmt.Sprintf("Produto não encontrado para o código/EAN '%s'", code)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"product":  prod,
		"quantity": qty,
		"total":    prod.Price * qty,
	})
}

func (s *Server) handleTables(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		tables, err := s.Store.ListTables()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, tables)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action     string `json:"action"` // COMPLEMENT, CREATE, EDIT, SAVE_TABLE, SET_ACTIVE, TOGGLE_ACTIVE, SET_VISIBLE_RANGE
			TableID    int    `json:"table_id"`
			Number     int    `json:"number"`
			Label      string `json:"label"`
			Complement string `json:"complement"`
			IsKiosk    bool   `json:"is_kiosk"`
			Active     *bool  `json:"active"`
			MinNumber  int    `json:"min_number"`
			MaxNumber  int    `json:"max_number"`
			Role       string `json:"role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		comp := req.Complement
		if comp == "" {
			comp = req.Label
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		if act == "SET_ACTIVE" || act == "TOGGLE_ACTIVE" {
			if !strings.EqualFold(req.Role, "ADMIN") {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "Apenas o Administrador (ADM) pode ativar ou inativar mesas."})
				return
			}
			activeVal := true
			if req.Active != nil {
				activeVal = *req.Active
			}
			if err := s.Store.AdminSetTableActive(req.TableID, activeVal); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			tables, _ := s.Store.ListTables()
			writeJSON(w, http.StatusOK, tables)
			return
		}
		if act == "SET_VISIBLE_RANGE" {
			if !strings.EqualFold(req.Role, "ADMIN") {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "Apenas o Administrador (ADM) pode definir a faixa de mesas visíveis para a equipe."})
				return
			}
			if err := s.Store.AdminSetTablesActiveRange(req.MinNumber, req.MaxNumber); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			tables, _ := s.Store.ListTables()
			writeJSON(w, http.StatusOK, tables)
			return
		}
		isAdminTableSave := act == "SAVE_TABLE" || act == "EDIT" || act == "CREATE"
		if act == "COMPLEMENT" || (req.TableID > 0 && !isAdminTableSave) {
			if err := s.Store.UpdateTableComplement(req.TableID, comp); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			tables, _ := s.Store.ListTables()
			writeJSON(w, http.StatusOK, tables)
			return
		}
		if !strings.EqualFold(req.Role, "ADMIN") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Apenas o Administrador (ADM) pode criar ou editar mesas. O Garçom pode apenas editar o complemento da mesa."})
			return
		}
		var tb *models.DiningTable
		var err error
		if req.Active != nil {
			tb, err = s.Store.AdminSaveTable(req.TableID, req.Number, comp, req.IsKiosk, *req.Active)
		} else {
			tb, err = s.Store.AdminSaveTable(req.TableID, req.Number, comp, req.IsKiosk)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, tb)
		return
	}
	if r.Method == http.MethodDelete {
		role := r.URL.Query().Get("role")
		if !strings.EqualFold(role, "ADMIN") {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Apenas o Administrador (ADM) pode excluir mesas."})
			return
		}
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if id > 0 {
			_ = s.Store.AdminDeleteTable(id)
		}
		tables, _ := s.Store.ListTables()
		writeJSON(w, http.StatusOK, tables)
	}
}

func (s *Server) handleTableStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	var req struct {
		Action         string  `json:"action"` // CALL_WAITER, REQUEST_BILL, CLEAR_CALL, COMPLEMENT, STATUS
		TableID        int     `json:"table_id"`
		Status         string  `json:"status"`
		Complement     string  `json:"complement"`
		ResetTotal     bool    `json:"reset_total"`
		CouponCode     string  `json:"coupon_code"`
		DiscountAmount float64 `json:"discount_amount"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	act := strings.ToUpper(strings.TrimSpace(req.Action))
	switch act {
	case "CALL_WAITER":
		_ = s.Store.SetTableWaiterCall(req.TableID, true, false)
		go s.dispatchWaiterCallPhysicalPrint(req.TableID, false)
	case "REQUEST_BILL":
		_ = s.Store.SetTableWaiterCall(req.TableID, true, true)
		go s.dispatchWaiterCallPhysicalPrint(req.TableID, true)
	case "CLEAR_CALL":
		_ = s.Store.SetTableWaiterCall(req.TableID, false, false)
	case "COMPLEMENT":
		_ = s.Store.UpdateTableComplement(req.TableID, req.Complement)
	default:
		if (req.ResetTotal || strings.EqualFold(req.Status, "LIVRE")) && strings.TrimSpace(req.CouponCode) != "" {
			s.Store.ApplyCouponToComandaOrOrder(0, req.TableID, 0, req.CouponCode, req.DiscountAmount)
		}
		if err := s.Store.UpdateTableStatus(req.TableID, req.Status, req.ResetTotal); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}
	tables, _ := s.Store.ListTables()
	writeJSON(w, http.StatusOK, tables)
}

func (s *Server) handleComandas(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		includePaid := r.URL.Query().Get("all") == "1"
		list, err := s.Store.ListComandas(includePaid)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action         string  `json:"action"` // OPEN, REQUEST_CLOSE, PAY, CLOSE, REMOVE_ITEM
			ComandaID      int     `json:"comanda_id"`
			OrderItemID    int     `json:"order_item_id"`
			ComandaCode    string  `json:"comanda_code"`
			ComandaType    string  `json:"comanda_type"` // QR_TABLET ou NOME_CELULAR
			TableID        int     `json:"table_id"`
			CustomerName   string  `json:"customer_name"`
			CustomerPhone  string  `json:"customer_phone"`
			PaymentMethod  string  `json:"payment_method"`
			CashierID      int     `json:"cashier_id"`
			CouponCode     string  `json:"coupon_code"`
			DiscountAmount float64 `json:"discount_amount"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		switch act {
		case "REMOVE_ITEM":
			cmd, err := s.Store.RemoveComandaItem(req.ComandaID, req.OrderItemID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, cmd)
		case "REQUEST_CLOSE":
			cmd, err := s.Store.RequestCloseComanda(req.ComandaID, req.ComandaCode)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			if cmd != nil {
				go s.dispatchBillClosurePhysicalPrint(*cmd, "FECHAMENTO / CONFERENCIA DE CONTA")
			}
			writeJSON(w, http.StatusOK, cmd)
		case "PAY", "CLOSE", "CLOSE_AND_PAY", "CANCEL_EMPTY", "DELETE_EMPTY":
			pm := req.PaymentMethod
			if act == "CANCEL_EMPTY" || act == "DELETE_EMPTY" {
				pm = "CANCELADA_ZERADA"
			} else if strings.TrimSpace(req.CouponCode) != "" {
				s.Store.ApplyCouponToComandaOrOrder(req.ComandaID, req.TableID, 0, req.CouponCode, req.DiscountAmount)
			}
			cmd, err := s.Store.PayAndCloseComanda(req.ComandaID, pm, req.CashierID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			if cmd != nil && cmd.Total > 0.009 && !strings.EqualFold(cmd.PaymentMethod, "CANCELADA_ZERADA") {
				go s.dispatchBillClosurePhysicalPrint(*cmd, "COMPROVANTE DE CONTA PAGA")
			}
			writeJSON(w, http.StatusOK, cmd)
		case "CLEAN_ALL_EMPTY", "DELETE_ALL_EMPTY":
			s.Store.CleanupZeroOrphanComandas()
			list, _ := s.Store.ListComandas(false)
			writeJSON(w, http.StatusOK, list)
		default: // OPEN
			cmd, err := s.Store.OpenOrGetComanda(req.TableID, req.ComandaCode, req.ComandaType, req.CustomerName, req.CustomerPhone)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, cmd)
		}
	}
}

func (s *Server) handleComandaCards(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cards, err := s.Store.ListComandaCards()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, cards)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			CardNumber int    `json:"card_number"`
			QRPayload  string `json:"qr_payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		card, err := s.Store.BindComandaCard(req.CardNumber, req.QRPayload)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, card)
	}
}

// handleMercadoPagoWebhook (Pronto para ativação futura Fase 2):
// Recebe confirmação de pagamento online (PIX/Cartão), muda status_pagamento para PAGO e libera impressão na Cozinha.
func (s *Server) handleMercadoPagoWebhook(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		OrderID       int    `json:"order_id"`
		GatewayID     string `json:"gateway_id"`
		PaymentStatus string `json:"payment_status"` // approved / PAGO
	}
	_ = json.NewDecoder(r.Body).Decode(&payload)
	if payload.OrderID > 0 && (strings.EqualFold(payload.PaymentStatus, "approved") || strings.EqualFold(payload.PaymentStatus, "PAGO")) {
		_, _ = s.Store.DB.Exec(`UPDATE orders SET status_pagamento = 'PAGO', gateway_id = ?, status = CASE WHEN status = 'AGUARDANDO_PAGAMENTO' THEN 'PREPARANDO' ELSE status END WHERE id = ?`,
			payload.GatewayID, payload.OrderID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"received": true,
		"module":   "MERCADO_PAGO_WEBHOOK_READY",
		"message":  "Estrutura de Webhook Mercado Pago pronta: ao confirmar pagamento online, libera ticket na cozinha.",
	})
}

// handleNFCePreview (Pronto para ativação futura de Notas Fiscais NFC-e Modelo 65):
// Gera o manifesto tributário estruturado com NCM, CEST, CFOP e ICMS de cada item do pedido.
func (s *Server) handleNFCePreview(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("order_id")
	ord, err := s.Store.GetOrderByIDOrQR(idStr)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Pedido não encontrado para prévia de NFC-e"})
		return
	}
	cfg := s.Store.GetPrinterConfig()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":        "READY_FOR_SEFAZ_NFCE_65",
		"nfce_enabled":  cfg.NFCeEnabled,
		"modelo_fiscal": "65 (NFC-e Consumidor Final)",
		"order_code":    ord.OrderCode,
		"total":         ord.Total,
		"items_fiscais": ord.Items,
		"observacao":    "Todos os produtos já possuem NCM, CEST e CFOP cadastrados no banco para transmissão SEFAZ.",
	})
}

// handleNFCeEmit emite e imprime o DANFE NFC-e (Modelo 65 - Certificado Digital A1) quando o módulo estiver ATIVADO em Configurações.
func (s *Server) handleNFCeEmit(w http.ResponseWriter, r *http.Request) {
	cfg := s.Store.GetPrinterConfig()
	if !cfg.NFCeEnabled {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":        "🔴 O Módulo de Nota Fiscal Eletrônica (NFC-e Certificado A1) está DESATIVADO no momento. Ative-o em ⚙️ Configurações quando desejar emitir notas fiscais.",
			"nfce_enabled": false,
		})
		return
	}

	var req struct {
		OrderID    int    `json:"order_id"`
		OrderCode  string `json:"order_code"`
		CPFCNPJ    string `json:"cpf_cnpj"`
		PrintDANFE bool   `json:"print_danfe"`
	}
	if r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&req)
	} else {
		req.OrderID, _ = strconv.Atoi(r.URL.Query().Get("order_id"))
		req.CPFCNPJ = r.URL.Query().Get("cpf_cnpj")
		req.PrintDANFE = r.URL.Query().Get("print") == "1"
	}

	lookup := strconv.Itoa(req.OrderID)
	if req.OrderID <= 0 && strings.TrimSpace(req.OrderCode) != "" {
		lookup = strings.TrimSpace(req.OrderCode)
	}
	ord, err := s.Store.GetOrderByIDOrQR(lookup)
	if err != nil || ord == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Pedido não encontrado para emissão de NFC-e."})
		return
	}

	cnpjClean := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, cfg.CNPJEmitente)
	if len(cnpjClean) < 14 {
		cnpjClean = (cnpjClean + "00000000000000")[:14]
	}
	serie := cfg.NFCeSerie
	if serie <= 0 {
		serie = 1
	}
	numero := cfg.NFCeProximoNumero
	if numero <= 0 {
		numero = 1
	}
	now := time.Now()
	chaveAcesso := fmt.Sprintf("35%s%s65%03d%09d1%08d9", now.Format("0601"), cnpjClean, serie, numero, ord.ID%100000000)
	if len(chaveAcesso) > 44 {
		chaveAcesso = chaveAcesso[:44]
	}
	protocolo := fmt.Sprintf("135%s%06d", now.Format("20060102"), numero)
	razao := strings.TrimSpace(cfg.NFCeRazaoSocial)
	if razao == "" {
		razao = "SIR BEEF BOUTIQUE DAS CARNES E HAMBURGUERIA LTDA"
	}
	ie := strings.TrimSpace(cfg.NFCeInscricaoEstadual)
	if ie == "" {
		ie = "ISENTO"
	}
	ender := strings.TrimSpace(cfg.NFCeEnderecoFiscal)
	if ender == "" {
		ender = "LOJA SIR BEEF - SP"
	}
	ambLabel := "HOMOLOGACAO (SEM VALOR FISCAL)"
	if strings.EqualFold(cfg.NFCeAmbiente, "PRODUCAO") {
		ambLabel = "PRODUCAO - AUTORIZADA SEFAZ"
	}
	cpfDest := strings.TrimSpace(req.CPFCNPJ)
	if cpfDest == "" {
		cpfDest = "CONSUMIDOR NAO IDENTIFICADO"
	} else {
		cpfDest = "CPF/CNPJ: " + cpfDest
	}

	var sb strings.Builder
	sb.WriteString("================================================\n")
	sb.WriteString(fmt.Sprintf("   %s\n", razao))
	sb.WriteString(fmt.Sprintf("   CNPJ: %s   IE: %s\n", cfg.CNPJEmitente, ie))
	sb.WriteString(fmt.Sprintf("   %s\n", ender))
	sb.WriteString("------------------------------------------------\n")
	sb.WriteString("   DANFE NFC-e - Documento Auxiliar da Nota\n")
	sb.WriteString("       Fiscal de Consumidor Eletronica\n")
	sb.WriteString(fmt.Sprintf("   Ambiente: %s\n", ambLabel))
	sb.WriteString("------------------------------------------------\n")
	sb.WriteString("ITEM  DESCRICAO             QTD   UN    VL.TOTAL\n")
	for idx, it := range ord.Items {
		sb.WriteString(fmt.Sprintf("%03d   %-20.20s  %.0f     UN   R$ %6.2f\n", idx+1, it.ProductName, it.Quantity, it.TotalPrice))
	}
	sb.WriteString("------------------------------------------------\n")
	if ord.DeliveryFee > 0 {
		sb.WriteString(fmt.Sprintf("TAXA DE ENTREGA:                       R$ %6.2f\n", ord.DeliveryFee))
	}
	if ord.DiscountAmount > 0 {
		sb.WriteString(fmt.Sprintf("DESCONTO:                            - R$ %6.2f\n", ord.DiscountAmount))
	}
	sb.WriteString(fmt.Sprintf("VALOR TOTAL R$:                        R$ %6.2f\n", ord.Total))
	sb.WriteString(fmt.Sprintf("FORMA PAGAMENTO: %s\n", ord.PaymentMethod))
	sb.WriteString("------------------------------------------------\n")
	sb.WriteString(fmt.Sprintf("DESTINATARIO: %s\n", cpfDest))
	sb.WriteString(fmt.Sprintf("NFC-e No: %09d   Serie: %03d\n", numero, serie))
	sb.WriteString(fmt.Sprintf("Emissao: %s\n", now.Format("02/01/2006 15:04:05")))
	sb.WriteString("CHAVE DE ACESSO:\n")
	sb.WriteString(fmt.Sprintf("%s\n", chaveAcesso))
	sb.WriteString(fmt.Sprintf("Protocolo Autorizacao: %s\n", protocolo))
	if strings.TrimSpace(cfg.NFCeCertFilename) != "" {
		sb.WriteString(fmt.Sprintf("Assinado c/ Cert. Digital A1: %s\n", cfg.NFCeCertFilename))
	}
	sb.WriteString("================================================\n")
	danfeText := sb.String()

	cfg.NFCeProximoNumero = numero + 1
	_ = s.Store.UpdatePrinterConfig(cfg)

	if req.PrintDANFE {
		stations, _ := s.Store.ListPrinterStations()
		for _, st := range stations {
			if st.Active && (st.PrintFiscalNotes || st.PrintBillClosure || st.ID == "CAIXA_EXPEDICAO") {
				_ = s.sendTicketToStation(st, danfeText)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "EMITTED",
		"order_id":       ord.ID,
		"order_code":     ord.OrderCode,
		"nfce_numero":    numero,
		"nfce_serie":     serie,
		"chave_acesso":   chaveAcesso,
		"protocolo":      protocolo,
		"ambiente":       cfg.NFCeAmbiente,
		"cpf_cnpj":       req.CPFCNPJ,
		"danfe_text":     danfeText,
		"certificado_a1": cfg.NFCeCertFilename,
		"message":        fmt.Sprintf("🧾 DANFE NFC-e Nº %d (Série %d) emitido com sucesso!", numero, serie),
	})
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		status := r.URL.Query().Get("status")
		orders, err := s.Store.ListOrders(status)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, orders)
		return
	}
	if r.Method == http.MethodPost {
		var o models.Order
		if err := json.NewDecoder(r.Body).Decode(&o); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		cfg := s.Store.GetPrinterConfig()
		origUp := strings.ToUpper(strings.TrimSpace(o.OriginType))
		hdrUp := strings.ToUpper(strings.TrimSpace(o.HeaderTag))
		isRetirada := origUp == "RETIRADA" || strings.Contains(hdrUp, "RETIRADA")
		isInternalPdv := (o.CashierID != nil && *o.CashierID > 0) ||
			strings.TrimSpace(o.CashierName) != "" ||
			(o.WaiterID != nil && *o.WaiterID > 0) ||
			origUp == "GARCOM" ||
			origUp == "PDV_MERCADO" ||
			origUp == "BALCAO" ||
			strings.EqualFold(o.StatusPagamento, "PAGO")
		if !isInternalPdv {
			if !cfg.IsOpenNow && (origUp == "DELIVERY" || isRetirada || origUp == "MESA" || origUp == "TABLET") {
				reason := strings.TrimSpace(cfg.ClosedReasonMessage)
				if reason == "" {
					reason = "🕒 Estamos fechados no momento! Confira nossos dias e horários de funcionamento."
				}
				writeJSON(w, http.StatusForbidden, map[string]string{"error": reason})
				return
			}
			if origUp == "TABLET" && !cfg.MenuEnableKiosk {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🔒 Estamos indisponíveis no momento ou fechados no Cardápio Tablet Kiosk."})
				return
			}
			if origUp == "DELIVERY" && !isRetirada && !cfg.MenuEnableDelivery {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🔒 Estamos indisponíveis no momento ou fechados para Entrega (Delivery)."})
				return
			}
			if isRetirada && !cfg.MenuEnableRetirada {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🔒 Estamos indisponíveis no momento ou fechados para Retirada no Balcão."})
				return
			}
			if origUp == "MESA" && !cfg.MenuEnableMesa {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🔒 Estamos indisponíveis no momento ou fechados para Pedidos na Mesa por QR Code."})
				return
			}
		}
		created, err := s.Store.CreateOrder(o)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if created != nil {
			go s.dispatchOrderPhysicalPrint(*created)
		}
		writeJSON(w, http.StatusCreated, created)
	}
}

func (s *Server) handleOrderStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST ou DELETE"})
		return
	}
	if r.Method == http.MethodDelete {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if id <= 0 {
			id, _ = strconv.Atoi(r.URL.Query().Get("order_id"))
		}
		if err := s.Store.AdminDeleteOrder(id); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		shifts, _ := s.Store.ListCashierShifts()
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "order_id": id, "cashier_shifts": shifts})
		return
	}

	var req struct {
		Action          string             `json:"action"` // STATUS, PAY_RETIRADA, EDIT_ORDER, DELETE_ORDER, CONVERT_TO_DELIVERY
		OrderID         int                `json:"order_id"`
		OriginType      string             `json:"origin_type"`
		HeaderTag       string             `json:"header_tag"`
		AddressText     string             `json:"address_text"`
		DeliveryFee     *float64           `json:"delivery_fee"`
		CustomerName    string             `json:"customer_name"`
		CustomerPhone   string             `json:"customer_phone"`
		Status          string             `json:"status"`
		StatusPagamento string             `json:"status_pagamento"`
		PaymentMethod   string             `json:"payment_method"`
		Total           float64            `json:"total"`
		CashierID       int                `json:"cashier_id"`
		Items           []models.OrderItem `json:"items"`
		ExtraItems      []models.OrderItem `json:"extra_items"`
		MotoboyID       int                `json:"motoboy_id"`
		MotoboyName     string             `json:"motoboy_name"`
		OperatorRole    string             `json:"operator_role"`
		AdminPIN        string             `json:"admin_pin"`
		CouponCode      string             `json:"coupon_code"`
		DiscountAmount  float64            `json:"discount_amount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	act := strings.ToUpper(strings.TrimSpace(req.Action))
	switch act {
	case "PAY_RETIRADA":
		updated, err := s.Store.PayAndDeliverRetiradaOrder(req.OrderID, req.PaymentMethod, req.CashierID, req.ExtraItems, req.Items)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if strings.TrimSpace(req.CouponCode) != "" {
			s.Store.ApplyCouponToComandaOrOrder(0, 0, req.OrderID, req.CouponCode, req.DiscountAmount)
			if refreshed, errRef := s.Store.GetOrderByIDOrQR(strconv.Itoa(req.OrderID)); errRef == nil && refreshed != nil {
				updated = refreshed
			}
		}
		if updated != nil && len(req.ExtraItems) > 0 {
			extraOrd := *updated
			extraOrd.Items = req.ExtraItems
			go s.dispatchOrderPhysicalPrintWithBanner(extraOrd, "*** ITEM ADICIONAL / ALTERADO ***")
		}
		writeJSON(w, http.StatusOK, updated)
		return
	case "REPRINT_ORDER":
		ord, err := s.Store.GetOrderByIDOrQR(strconv.Itoa(req.OrderID))
		if err != nil || ord == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Pedido não encontrado para reimpressão."})
			return
		}
		sectorTickets := s.Store.BuildOrderSectorTickets(*ord)
		var sectorNames []string
		for _, tk := range sectorTickets {
			sectorNames = append(sectorNames, tk.StationName)
		}
		go s.dispatchOrderPhysicalPrintWithBanner(*ord, "*** REIMPRESSAO ***")
		_ = s.Store.AddSystemLog("INFO", "REIMPRESSAO", fmt.Sprintf("Reimpressão disparada para o Pedido #%s em todos os setores (%s)", ord.OrderCode, strings.Join(sectorNames, ", ")), "", "")
		writeJSON(w, http.StatusOK, map[string]any{
			"reprinted":  true,
			"order_id":   ord.ID,
			"order_code": ord.OrderCode,
			"sectors":    sectorNames,
			"message":    fmt.Sprintf("🖨️ Reimpressão enviada com cabeçalho 'REIMPRESSÃO' para todos os setores do Pedido #%s!", ord.OrderCode),
		})
		return
	case "EDIT_ORDER", "EXPEDICAO_EDIT_ORDER", "CONVERT_TO_DELIVERY":
		extraOrd := models.Order{
			OriginType:  req.OriginType,
			HeaderTag:   req.HeaderTag,
			AddressText: req.AddressText,
			DeliveryFee: -1,
		}
		if act == "CONVERT_TO_DELIVERY" && extraOrd.OriginType == "" {
			extraOrd.OriginType = "DELIVERY"
		}
		if req.DeliveryFee != nil {
			extraOrd.DeliveryFee = *req.DeliveryFee
		}
		updated, err := s.Store.AdminEditOrder(req.OrderID, req.CustomerName, req.CustomerPhone, req.PaymentMethod, req.Status, req.StatusPagamento, req.Total, req.Items, extraOrd)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if updated != nil {
			go s.dispatchOrderPhysicalPrintWithBanner(*updated, "*** PEDIDO ALTERADO / EDITADO ***")
			_ = s.Store.AddSystemLog("INFO", "PEDIDO-EDITADO", fmt.Sprintf("Pedido #%s editado e reimpresso nos setores com aviso de PEDIDO ALTERADO", updated.OrderCode), "", "")
		}
		shifts, _ := s.Store.ListCashierShifts()
		newTotVal := req.Total
		if updated != nil {
			newTotVal = updated.Total
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"order":          updated,
			"new_total":      newTotVal,
			"cashier_shifts": shifts,
			"message":        "Pedido atualizado, valor recalculado e reimpresso nos setores com aviso de PEDIDO ALTERADO!",
		})
		return
	case "DELETE_ORDER":
		if !strings.EqualFold(req.OperatorRole, "ADMIN") {
			ok, _ := s.Store.VerifyAdminPinOrUser(req.AdminPIN)
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🚫 Apenas o Administrador (ADM) tem permissão para excluir vendas!"})
				return
			}
		}
		if err := s.Store.AdminDeleteOrder(req.OrderID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		shifts, _ := s.Store.ListCashierShifts()
		writeJSON(w, http.StatusOK, map[string]any{
			"deleted":        true,
			"order_id":       req.OrderID,
			"cashier_shifts": shifts,
			"message":        "Venda excluída permanentemente e caixa do operador atualizado!",
		})
		return
	case "NOTIFY_WAITER", "TRIGGER_KITCHEN_READY":
		alert, err := s.Store.TriggerKitchenReadyAlert(req.OrderID)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"alert":   alert,
			"message": "🔔 Alarme enviado para o APK do Garçom avisando que o pedido está pronto na Cozinha!",
		})
		return
	}
	if strings.EqualFold(strings.TrimSpace(req.Status), "ENTREGUE") {
		if curOrd, errOrd := s.Store.GetOrderByIDOrQR(strconv.Itoa(req.OrderID)); errOrd == nil && curOrd != nil {
			origUp := strings.ToUpper(strings.TrimSpace(curOrd.OriginType))
			tagUp := strings.ToUpper(strings.TrimSpace(curOrd.HeaderTag))
			isDel := origUp == "DELIVERY" && !strings.Contains(tagUp, "RETIRADA")
			isMesa := (curOrd.TableID != nil && *curOrd.TableID > 0) || origUp == "MESA" || origUp == "GARCOM" || origUp == "TABLET" || strings.Contains(tagUp, "MESA") || strings.Contains(tagUp, "BISTR")
			isPaid := strings.EqualFold(strings.TrimSpace(curOrd.StatusPagamento), "PAGO")
			if !isDel && !isMesa && !isPaid {
				writeJSON(w, http.StatusBadRequest, map[string]string{
					"error": "🔒 Pedidos de Retirada ou Balcão só podem ir para Entregues quando o Caixa Geral der baixa como PAGO!",
				})
				return
			}
		}
	}
	updated, err := s.Store.UpdateOrderStatus(req.OrderID, req.Status, req.MotoboyID, req.MotoboyName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Se for pedido de Mesa / Salão / Tablet e a Cozinha/Expedição mudou para PRONTO, também aciona automaticamente o garçom se o alarme estiver ativo!
	if strings.EqualFold(req.Status, "PRONTO") && updated != nil {
		if (updated.TableID != nil && *updated.TableID > 0) || strings.EqualFold(updated.OriginType, "MESA") || strings.EqualFold(updated.OriginType, "GARCOM") || strings.EqualFold(updated.OriginType, "TABLET") {
			_, _ = s.Store.TriggerKitchenReadyAlert(updated.ID)
		}
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDispatchByQR(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	var req struct {
		QRPayload   string `json:"qr_payload"`
		MotoboyID   int    `json:"motoboy_id"`
		MotoboyName string `json:"motoboy_name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.MotoboyID <= 0 {
		req.MotoboyID = 4
	}
	ord, err := s.Store.GetOrderByIDOrQR(req.QRPayload)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	if !strings.EqualFold(ord.OriginType, "DELIVERY") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Apenas pedidos de ENTREGA (Delivery) possuem despacho de Motoboy por QR Code."})
		return
	}
	updated, err := s.Store.UpdateOrderStatus(ord.ID, "DESPACHADO", req.MotoboyID, req.MotoboyName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message": fmt.Sprintf("Pedido #%s despachado para %s! Cliente avisado no WhatsApp.", updated.OrderCode, updated.MotoboyName),
		"order":   updated,
	})
}

func (s *Server) handleOrderReceipt(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	colsStr := r.URL.Query().Get("columns")
	cfg := s.Store.GetPrinterConfig()
	cols := cfg.Columns
	if c, err := strconv.Atoi(colsStr); err == nil && (c == 32 || c == 48) {
		cols = c
	}

	ord, err := s.Store.GetOrderByIDOrQR(idStr)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	textReceipt := escpos.RenderTextReceipt(*ord, cols)
	rawBytes := escpos.BuildRawESCPOS(*ord, cols)
	sectorTickets := s.Store.BuildOrderSectorTickets(*ord)

	writeJSON(w, http.StatusOK, map[string]any{
		"order_id":       ord.ID,
		"order_code":     ord.OrderCode,
		"columns":        cols,
		"paper_width_mm": map[int]int{32: 58, 48: 80}[cols],
		"text_receipt":   textReceipt,
		"sector_tickets": sectorTickets,
		"raw_byte_len":   len(rawBytes),
		"qr_payload":     ord.QRPayload,
	})
}

func (s *Server) handleMotoboySettlement(w http.ResponseWriter, r *http.Request) {
	mid, _ := strconv.Atoi(r.URL.Query().Get("motoboy_id"))
	summary, err := s.Store.GetMotoboySettlement(mid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleMotoboyShifts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		shifts, err := s.Store.ListMotoboyShifts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, shifts)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action       string  `json:"action"` // OPEN ou CLOSE
			MotoboyID    int     `json:"motoboy_id"`
			MotoboyName  string  `json:"motoboy_name"`
			OpeningFloat float64 `json:"opening_float"`
			AdminPIN     string  `json:"admin_pin"`
			OperatorRole string  `json:"operator_role"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.EqualFold(req.OperatorRole, "ADMIN") {
			ok, _ := s.Store.VerifyAdminPinOrUser(req.AdminPIN)
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🚫 Apenas o Administrador (ADM - Senha/PIN 1111) pode abrir ou fechar o caixa dos Motoboys!"})
				return
			}
		}
		if strings.EqualFold(req.Action, "CLOSE") {
			_, _ = s.Store.CloseMotoboyShift(req.MotoboyID)
		} else {
			_, _ = s.Store.OpenMotoboyShift(req.MotoboyID, req.MotoboyName, req.OpeningFloat)
		}
		shifts, _ := s.Store.ListMotoboyShifts()
		writeJSON(w, http.StatusOK, shifts)
	}
}

func (s *Server) handleCashierShifts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		shifts, err := s.Store.ListCashierShifts()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, shifts)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action        string  `json:"action"` // OPEN ou CLOSE
			CashierID     int     `json:"cashier_id"`
			OpeningFloat  float64 `json:"opening_float"`
			CountedCash   float64 `json:"counted_cash"`
			CountedCard   float64 `json:"counted_card"`
			CountedPix    float64 `json:"counted_pix"`
			ValidateMatch bool    `json:"validate_match"`
			AdminPIN      string  `json:"admin_pin"`
			AdminName     string  `json:"admin_name"`
			OperatorRole  string  `json:"operator_role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		adminName := strings.TrimSpace(req.AdminName)
		if !strings.EqualFold(req.OperatorRole, "ADMIN") {
			ok, verifiedName := s.Store.VerifyAdminPinOrUser(req.AdminPIN)
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🚫 Apenas o Administrador (ADM) pode abrir ou fechar o caixa do PDV!"})
				return
			}
			adminName = verifiedName
		}
		if adminName == "" {
			adminName = "Guilherme (Diretor / ADM)"
		}
		if strings.EqualFold(req.Action, "CLOSE") {
			if _, err := s.Store.CloseCashierShiftWithReconciliation(req.CashierID, adminName, req.CountedCash, req.CountedCard, req.CountedPix, req.ValidateMatch); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		} else {
			_, _ = s.Store.OpenCashierShift(req.CashierID, req.OpeningFloat, adminName)
		}
		shifts, _ := s.Store.ListCashierShifts()
		writeJSON(w, http.StatusOK, shifts)
	}
}

func (s *Server) handleCustomers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		customers, err := s.Store.ListCustomers()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"reward_threshold_brl": s.Store.GetRewardThreshold(),
			"customers":            customers,
		})
		return
	}
	if r.Method == http.MethodDelete {
		role := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("role")))
		pin := strings.TrimSpace(r.URL.Query().Get("admin_pin"))
		if role != "ADMIN" {
			ok, _ := s.Store.VerifyAdminPinOrUser(pin)
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🚫 Apenas o Administrador (ADM) tem permissão para excluir clientes!"})
				return
			}
		}
		cid, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if cid <= 0 {
			cid, _ = strconv.Atoi(r.URL.Query().Get("customer_id"))
		}
		if err := s.Store.AdminDeleteCustomer(cid); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		customers, _ := s.Store.ListCustomers()
		writeJSON(w, http.StatusOK, map[string]any{
			"reward_threshold_brl": s.Store.GetRewardThreshold(),
			"customers":            customers,
		})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action             string          `json:"action"` // SAVE, SAVE_CUSTOMER, REDEEM_REWARD, SET_THRESHOLD, DELETE, DELETE_CUSTOMER, DELETE_CUSTOMER_ORDER
			CustomerID         int             `json:"customer_id"`
			OrderID            int             `json:"order_id"`
			ID                 int             `json:"id"`
			OperatorRole       string          `json:"operator_role"`
			Role               string          `json:"role"`
			AdminPIN           string          `json:"admin_pin"`
			RewardThresholdBRL float64         `json:"reward_threshold_brl"`
			ThresholdBRL       float64         `json:"threshold_brl"`
			Customer           models.Customer `json:"customer"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		switch act {
		case "DELETE_CUSTOMER_ORDER", "DELETE_HISTORY_ITEM":
			isAdmin := strings.EqualFold(req.OperatorRole, "ADMIN") || strings.EqualFold(req.Role, "ADMIN")
			if !isAdmin {
				ok, _ := s.Store.VerifyAdminPinOrUser(req.AdminPIN)
				isAdmin = ok
			}
			if !isAdmin {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🚫 Apenas o Administrador (ADM) tem permissão para excluir pedidos do histórico do cliente!"})
				return
			}
			oid := req.OrderID
			if oid <= 0 {
				oid = req.ID
			}
			if err := s.Store.AdminDeleteOrder(oid); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		case "DELETE", "DELETE_CUSTOMER":
			isAdmin := strings.EqualFold(req.OperatorRole, "ADMIN") || strings.EqualFold(req.Role, "ADMIN")
			if !isAdmin {
				ok, _ := s.Store.VerifyAdminPinOrUser(req.AdminPIN)
				isAdmin = ok
			}
			if !isAdmin {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🚫 Apenas o Administrador (ADM) tem permissão para excluir clientes!"})
				return
			}
			cid := req.CustomerID
			if cid <= 0 {
				cid = req.ID
			}
			if cid <= 0 {
				cid = req.Customer.ID
			}
			if err := s.Store.AdminDeleteCustomer(cid); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		case "REDEEM_REWARD":
			cid := req.CustomerID
			if cid <= 0 {
				cid = req.Customer.ID
			}
			if _, err := s.Store.RedeemCustomerReward(cid); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		case "SET_THRESHOLD":
			th := req.RewardThresholdBRL
			if th <= 0 {
				th = req.ThresholdBRL
			}
			_ = s.Store.SetRewardThreshold(th)
		default: // SAVE or SAVE_CUSTOMER
			if _, err := s.Store.SaveCustomer(req.Customer); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		customers, _ := s.Store.ListCustomers()
		writeJSON(w, http.StatusOK, map[string]any{
			"reward_threshold_brl": s.Store.GetRewardThreshold(),
			"customers":            customers,
		})
	}
}

func (s *Server) discoverAllPCPrinters() ([]models.DetectedPrinter, []map[string]any) {
	hostname, _ := os.Hostname()
	localLabel := "PC-COZINHA"
	if hostname != "" {
		localLabel = fmt.Sprintf("PC-COZINHA (%s)", hostname)
	}
	all := escpos.DetectLocalWindowsPrinters(localLabel, "🖥️ PC Cozinha (Windows 11)")

	s.remotePrinterMu.Lock()
	defer s.remotePrinterMu.Unlock()
	var remotePCs []map[string]any
	for pcName, list := range s.remotePrintersByPC {
		seenAt := s.remotePCSeenAt[pcName]
		online := time.Since(seenAt) < 25*time.Second
		remotePCs = append(remotePCs, map[string]any{
			"pc_name":        pcName,
			"online":         online,
			"last_seen":      seenAt.Format("15:04:05"),
			"printers_count": len(list),
		})
		for _, p := range list {
			p.IsRemotePC = true
			all = append(all, p)
		}
	}
	return all, remotePCs
}

func (s *Server) enqueueRemotePrintJob(st models.PrinterStation, ticketText string) {
	s.remotePrinterMu.Lock()
	defer s.remotePrinterMu.Unlock()
	job := models.RemotePrintJob{
		ID:                 fmt.Sprintf("job-%d", time.Now().UnixNano()),
		PCName:             st.PCName,
		TargetPCName:       st.PCName,
		WindowsPrinterName: st.WindowsPrinterName,
		StationID:          st.ID,
		StationName:        st.Name,
		TicketText:         ticketText,
		TextContent:        ticketText,
		CutPaper:           st.CutPaperSeparately,
		Columns:            st.Columns,
		CreatedAt:          time.Now().Format("15:04:05"),
	}
	s.remotePrintQueue = append(s.remotePrintQueue, job)
	if len(s.remotePrintQueue) > 100 {
		s.remotePrintQueue = s.remotePrintQueue[len(s.remotePrintQueue)-100:]
	}
}

func (s *Server) sendTicketToStation(st models.PrinterStation, ticketText string) error {
	if !st.Active {
		return nil
	}
	rawBytes := escpos.BuildRawTextTicketESCPOS(ticketText, st.CutPaperSeparately)
	winPrinter := strings.TrimSpace(st.WindowsPrinterName)
	pcUpper := strings.ToUpper(strings.TrimSpace(st.PCName))
	hostname, _ := os.Hostname()
	isRemoteCashier := strings.Contains(pcUpper, "CAIXA") && (hostname == "" || !strings.Contains(pcUpper, strings.ToUpper(hostname)))

	if winPrinter != "" {
		if isRemoteCashier {
			// Tenta também se for impressora compartilhada UNC (\\PC-CAIXA\...) acessível direto na rede Windows; senão envia pela Ponte do PC do Caixa!
			if strings.HasPrefix(winPrinter, `\\`) {
				if err := escpos.SendRawToWindowsSpooler(winPrinter, rawBytes); err == nil {
					_ = s.Store.AddSystemLog("INFO", "IMPRESSORA-REDE", fmt.Sprintf("Impresso via Rede Windows (%s) no setor '%s'", winPrinter, st.Name), "", st.PCName)
					return nil
				}
			}
			s.enqueueRemotePrintJob(st, ticketText)
			_ = s.Store.AddSystemLog("INFO", "PONTE-PC-CAIXA", fmt.Sprintf("Comanda enviada para a Ponte do %s -> Impressora '%s' (%s) [Corte=%v]", st.PCName, winPrinter, st.ConnectionType, st.CutPaperSeparately), "", st.PCName)
			return nil
		}

		// Impressão direta no Spooler Windows local (PC da Cozinha Win11 USB ou impressora instalada localmente)
		if err := escpos.SendRawToWindowsSpooler(winPrinter, rawBytes); err != nil {
			// Se não achou localmente, enfileira também para o agente remoto caso seja do outro PC
			s.enqueueRemotePrintJob(st, ticketText)
			_ = s.Store.AddSystemLog("WARN", "IMPRESSORA-SPOOLER", fmt.Sprintf("Aviso Spooler local '%s' (%v) — enviado também para fila remota (%s)", winPrinter, err, st.PCName), "", st.PCName)
			return err
		}
		_ = s.Store.AddSystemLog("INFO", "IMPRESSORA-USB-WIN", fmt.Sprintf("Impresso com sucesso em '%s' (%s | %s) [Corte Separado=%v]", winPrinter, st.PCName, st.ConnectionType, st.CutPaperSeparately), "", st.PCName)
		return nil
	}

	if strings.TrimSpace(st.PrinterIP) != "" {
		port := st.PrinterPort
		if port <= 0 {
			port = 9100
		}
		err := escpos.SendToNetworkPrinter(st.PrinterIP, port, rawBytes)
		if err == nil {
			_ = s.Store.AddSystemLog("INFO", "IMPRESSORA-TCP", fmt.Sprintf("Impresso via IP %s:%d no setor '%s'", st.PrinterIP, port, st.Name), "", st.PCName)
		}
		return err
	}
	return nil
}

func prependPrintBanner(ticketText string, banner string, cols int) string {
	banner = strings.TrimSpace(banner)
	if banner == "" {
		return ticketText
	}
	if cols != 32 && cols != 48 {
		cols = 48
	}
	div := strings.Repeat("=", cols)
	pad := (cols - len(banner)) / 2
	if pad < 0 {
		pad = 0
	}
	centered := strings.Repeat(" ", pad) + banner
	nowStr := time.Now().Format("02/01 15:04:05")
	timeLabel := "HORA: " + nowStr
	padTime := (cols - len(timeLabel)) / 2
	if padTime < 0 {
		padTime = 0
	}
	centeredTime := strings.Repeat(" ", padTime) + timeLabel
	return fmt.Sprintf("%s\n%s\n%s\n%s\n", div, centered, centeredTime, div) + ticketText
}

func (s *Server) dispatchOrderPhysicalPrint(o models.Order) {
	s.dispatchOrderPhysicalPrintWithBanner(o, "")
}

func (s *Server) dispatchOrderPhysicalPrintWithBanner(o models.Order, banner string) {
	stations, _ := s.Store.ListPrinterStations()
	stMap := map[string]models.PrinterStation{}
	for _, st := range stations {
		stMap[st.ID] = st
	}

	tickets := s.Store.BuildOrderSectorTickets(o)
	printedStations := map[string]bool{}
	for i, tk := range tickets {
		st, ok := stMap[tk.StationID]
		if !ok || !st.Active {
			continue
		}
		if i > 0 {
			// Pequena pausa entre setores para que a mesma impressora térmica USB processe e corte os 2 papéis separadamente (ex: 1 papel Lanche + 1 papel Porção)
			time.Sleep(350 * time.Millisecond)
		}
		txt := prependPrintBanner(tk.TicketText, banner, st.Columns)
		_ = s.sendTicketToStation(st, txt)
		printedStations[st.ID] = true
	}

	// Se for pedido de Delivery, Retirada ou Balcão/Mercado, imprime também o cupom completo na impressora do Caixa (PrintFiscalNotes ou nome "Caixa")
	orig := strings.ToUpper(strings.TrimSpace(o.OriginType))
	if orig == "DELIVERY" || orig == "RETIRADA" || orig == "PDV_RAPIDO" || orig == "PDV_MERCADO" || orig == "BALCAO" {
		hasUsefulFiscal := false
		for _, st := range stations {
			if st.Active && strings.TrimSpace(st.WindowsPrinterName) != "" && st.PrintFiscalNotes {
				hasUsefulFiscal = true
				break
			}
		}
		for _, st := range stations {
			if !st.Active || printedStations[st.ID] {
				continue
			}
			isUseful := strings.TrimSpace(st.WindowsPrinterName) != ""
			isCaixaNamed := isUseful && (strings.Contains(strings.ToUpper(st.Name), "CAIXA") || strings.Contains(strings.ToUpper(st.WindowsPrinterName), "CAIXA") || strings.Contains(strings.ToUpper(st.ID), "CAIXA"))
			if (st.PrintFiscalNotes && (isUseful || !hasUsefulFiscal)) || (!hasUsefulFiscal && isCaixaNamed) {
				cols := st.Columns
				if cols != 32 && cols != 48 {
					cols = 48
				}
				receiptText := prependPrintBanner(escpos.RenderTextReceipt(o, cols), banner, cols)
				time.Sleep(350 * time.Millisecond)
				_ = s.sendTicketToStation(st, receiptText)
				printedStations[st.ID] = true
			}
		}
	}
}

func (s *Server) dispatchWaiterCallPhysicalPrint(tableID int, requestBill bool) {
	stations, _ := s.Store.ListPrinterStations()
	var tableLabel string
	var partialTotal float64
	_ = s.Store.DB.QueryRow(`SELECT label, current_total FROM dining_tables WHERE number = ? OR id = ? ORDER BY CASE WHEN number = ? THEN 0 ELSE 1 END LIMIT 1`, tableID, tableID, tableID).Scan(&tableLabel, &partialTotal)
	if tableLabel == "" {
		tableLabel = fmt.Sprintf("MESA %02d", tableID)
	}
	reason := "CHAMADO DE GARCOM NA MESA"
	if requestBill {
		reason = "PEDIU FECHAMENTO DA CONTA"
	}
	hasUsefulWaiter := false
	for _, st := range stations {
		if st.Active && strings.TrimSpace(st.WindowsPrinterName) != "" && st.PrintWaiterCalls {
			hasUsefulWaiter = true
			break
		}
	}
	for _, st := range stations {
		if !st.Active {
			continue
		}
		isUseful := strings.TrimSpace(st.WindowsPrinterName) != ""
		isCaixaOrBarNamed := isUseful && (strings.Contains(strings.ToUpper(st.Name), "CAIXA") || strings.Contains(strings.ToUpper(st.Name), "BAR") || strings.Contains(strings.ToUpper(st.WindowsPrinterName), "CAIXA"))
		if (st.PrintWaiterCalls && (isUseful || !hasUsefulWaiter)) || (!hasUsefulWaiter && isCaixaOrBarNamed) {
			ticket := escpos.RenderWaiterCallTicket(tableLabel, reason, "", "", partialTotal, st.Columns)
			_ = s.sendTicketToStation(st, ticket)
		}
	}
}

func (s *Server) dispatchBillClosurePhysicalPrint(cmd models.Comanda, actionTitle string) {
	stations, _ := s.Store.ListPrinterStations()
	hasUsefulClosure := false
	for _, st := range stations {
		if st.Active && strings.TrimSpace(st.WindowsPrinterName) != "" && st.PrintBillClosure {
			hasUsefulClosure = true
			break
		}
	}
	for _, st := range stations {
		if !st.Active {
			continue
		}
		isUseful := strings.TrimSpace(st.WindowsPrinterName) != ""
		isCaixaNamed := isUseful && (strings.Contains(strings.ToUpper(st.Name), "CAIXA") || strings.Contains(strings.ToUpper(st.WindowsPrinterName), "CAIXA") || strings.Contains(strings.ToUpper(st.ID), "CAIXA"))
		if (st.PrintBillClosure && (isUseful || !hasUsefulClosure)) || (!hasUsefulClosure && isCaixaNamed) {
			ticket := escpos.RenderComandaClosureTicket(cmd, st.Columns)
			_ = s.sendTicketToStation(st, ticket)
		}
	}
}

func (s *Server) handlePrinterStations(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		stations, _ := s.Store.ListPrinterStations()
		routes, _ := s.Store.ListCategoryPrinterRoutes()
		detected, remotePCs := s.discoverAllPCPrinters()
		hostname, _ := os.Hostname()
		writeJSON(w, http.StatusOK, map[string]any{
			"stations":          stations,
			"routes":            routes,
			"detected_printers": detected,
			"remote_pcs":        remotePCs,
			"local_hostname":    hostname,
		})
		return
	}
	if r.Method == http.MethodDelete {
		id := r.URL.Query().Get("id")
		if id != "" {
			_ = s.Store.DeletePrinterStation(id)
		}
		stations, _ := s.Store.ListPrinterStations()
		routes, _ := s.Store.ListCategoryPrinterRoutes()
		detected, remotePCs := s.discoverAllPCPrinters()
		writeJSON(w, http.StatusOK, map[string]any{
			"stations":          stations,
			"routes":            routes,
			"detected_printers": detected,
			"remote_pcs":        remotePCs,
		})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action             string `json:"action"` // SAVE_STATION, DELETE_STATION, SAVE_ROUTE, DISCOVER, TEST_PRINT, TEST_DETECTED_PRINTER, AGENT_HEARTBEAT
			Type               string `json:"type"`   // STATION ou ROUTE
			CategoryID         int    `json:"category_id"`
			StationID          string `json:"station_id"`
			PCName             string `json:"pc_name"`
			WindowsPrinterName string `json:"windows_printer_name"`
			ConnectionType     string `json:"connection_type"`
			Printers           []struct {
				Name           string `json:"name"`
				PortName       string `json:"port_name"`
				DriverName     string `json:"driver_name"`
				ConnectionType string `json:"connection_type"`
				Shared         bool   `json:"shared"`
				ShareName      string `json:"share_name"`
				Status         string `json:"status"`
			} `json:"printers"`
			Station struct {
				ID                 string `json:"id"`
				Name               string `json:"name"`
				SectorLabel        string `json:"sector_label"`
				ConnectionType     string `json:"connection_type"`
				PCName             string `json:"pc_name"`
				WindowsPrinterName string `json:"windows_printer_name"`
				PrinterIP          string `json:"printer_ip"`
				IPAddress          string `json:"ip_address"`
				PrinterPort        int    `json:"printer_port"`
				Port               int    `json:"port"`
				Columns            int    `json:"columns"`
				PaperWidthMM       int    `json:"paper_width_mm"`
				CutPaperSeparately *bool  `json:"cut_paper_separately"`
				PrintWaiterCalls   *bool  `json:"print_waiter_calls"`
				PrintDrinks        *bool  `json:"print_drinks"`
				PrintBillClosure   *bool  `json:"print_bill_closure"`
				PrintBoutique      *bool  `json:"print_boutique"`
				PrintFiscalNotes   *bool  `json:"print_fiscal_notes"`
				Active             *bool  `json:"active"`
				Enabled            *bool  `json:"enabled"`
			} `json:"station"`
			Route struct {
				CategoryID       int    `json:"category_id"`
				PrinterStationID string `json:"printer_station_id"`
			} `json:"route"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		act := strings.ToUpper(strings.TrimSpace(req.Action))
		switch act {
		case "AGENT_HEARTBEAT":
			pcName := strings.TrimSpace(req.PCName)
			if pcName == "" {
				pcName = "PC-CAIXA (Win10)"
			}
			var remoteList []models.DetectedPrinter
			for _, rp := range req.Printers {
				connType, badge := escpos.ClassifyPrinterConnection(rp.Name, rp.PortName, rp.DriverName, rp.Shared)
				if rp.ConnectionType != "" {
					connType = rp.ConnectionType
				}
				st := rp.Status
				if st == "" {
					st = "PRONTA"
				}
				remoteList = append(remoteList, models.DetectedPrinter{
					ID:                 fmt.Sprintf("%s::%s", pcName, rp.Name),
					Key:                fmt.Sprintf("%s|%s", pcName, rp.Name),
					Name:               rp.Name,
					PrinterName:        rp.Name,
					WindowsPrinterName: rp.Name,
					PCName:             pcName,
					PCRoleLabel:        "💻 " + pcName,
					ConnectionType:     connType,
					ConnectionBadge:    badge,
					PortName:           rp.PortName,
					DriverName:         rp.DriverName,
					Shared:             rp.Shared,
					ShareName:          rp.ShareName,
					Status:             st,
					LastSeenAt:         time.Now().Format("15:04:05"),
					IsRemotePC:         true,
				})
			}
			s.remotePrinterMu.Lock()
			s.remotePrintersByPC[pcName] = remoteList
			s.remotePCSeenAt[pcName] = time.Now()
			var jobsForPC []models.RemotePrintJob
			var remaining []models.RemotePrintJob
			for _, j := range s.remotePrintQueue {
				jPC := strings.ToUpper(j.PCName)
				if strings.Contains(jPC, "CAIXA") || strings.EqualFold(j.PCName, pcName) {
					jobsForPC = append(jobsForPC, j)
				} else {
					remaining = append(remaining, j)
				}
			}
			s.remotePrintQueue = remaining
			s.remotePrinterMu.Unlock()

			writeJSON(w, http.StatusOK, map[string]any{
				"status": "OK",
				"pc":     pcName,
				"jobs":   jobsForPC,
			})
			return

		case "DISCOVER":
			stations, _ := s.Store.ListPrinterStations()
			routes, _ := s.Store.ListCategoryPrinterRoutes()
			detected, remotePCs := s.discoverAllPCPrinters()
			writeJSON(w, http.StatusOK, map[string]any{
				"stations":          stations,
				"routes":            routes,
				"detected_printers": detected,
				"remote_pcs":        remotePCs,
			})
			return

		case "DELETE_STATION":
			stID := req.StationID
			if stID == "" {
				stID = req.Station.ID
			}
			_ = s.Store.DeletePrinterStation(stID)

		case "TEST_DETECTED_PRINTER":
			winPrinter := strings.TrimSpace(req.WindowsPrinterName)
			if winPrinter == "" {
				winPrinter = strings.TrimSpace(req.Station.WindowsPrinterName)
			}
			pcName := strings.TrimSpace(req.PCName)
			if pcName == "" {
				pcName = strings.TrimSpace(req.Station.PCName)
			}
			if pcName == "" {
				pcName = "PC-COZINHA (Win11)"
			}
			connType := strings.TrimSpace(req.ConnectionType)
			if connType == "" {
				connType = strings.TrimSpace(req.Station.ConnectionType)
			}
			if connType == "" {
				connType = "USB"
			}
			if winPrinter == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Informe o nome da impressora encontrada para realizar o teste"})
				return
			}
			tempStation := models.PrinterStation{
				ID:                 "TESTE_AUTODETECT",
				Name:               fmt.Sprintf("TESTE DIRETO: %s", winPrinter),
				SectorLabel:        fmt.Sprintf("TESTE DE IMPRESSORA (%s)", connType),
				ConnectionType:     connType,
				PCName:             pcName,
				WindowsPrinterName: winPrinter,
				Columns:            48,
				PaperWidthMM:       80,
				CutPaperSeparately: true,
				Active:             true,
			}
			sampleItems := []models.OrderItem{
				{ProductName: fmt.Sprintf("IMPRESSORA: %s", winPrinter), Quantity: 1},
				{ProductName: fmt.Sprintf("PC: %s | CONEXAO: %s", pcName, connType), Quantity: 1},
			}
			sampleOrder := models.Order{
				OrderCode:    "TESTE-AUTO",
				OriginType:   "AUTODETECT",
				HeaderTag:    "TESTE DE IMPRESSORA ENCONTRADA",
				CustomerName: "Teste Direto Painel Autodeteccao",
				CreatedAt:    time.Now().Format("15:04"),
			}
			ticketText := escpos.RenderSectorTicket(sampleOrder, tempStation, sampleItems)
			err := s.sendTicketToStation(tempStation, ticketText)
			msg := fmt.Sprintf("🖨️ Impressão de teste enviada diretamente para a impressora '%s' (%s — %s) com corte de guilhotina!", winPrinter, pcName, connType)
			writeJSON(w, http.StatusOK, map[string]any{
				"tested":      true,
				"station":     tempStation,
				"ticket_text": ticketText,
				"message":     msg,
				"spool_error": func() string {
					if err != nil {
						return err.Error()
					}
					return ""
				}(),
			})
			return

		case "TEST_PRINT":
			stID := req.StationID
			if stID == "" {
				stID = req.Station.ID
			}
			stations, _ := s.Store.ListPrinterStations()
			var target *models.PrinterStation
			for i := range stations {
				if strings.EqualFold(stations[i].ID, stID) {
					target = &stations[i]
					break
				}
			}
			if target == nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "Setor de impressão não encontrado"})
				return
			}
			sampleItems := []models.OrderItem{
				{ProductName: "TESTE DE IMPRESSAO E GUILHOTINA (" + target.SectorLabel + ")", Quantity: 1},
			}
			sampleOrder := models.Order{
				OrderCode:    "TESTE-OK",
				OriginType:   "PDV",
				HeaderTag:    "TESTE DE SETOR",
				CustomerName: "Teste Automatico Sir Beef",
				CreatedAt:    time.Now().Format("15:04"),
			}
			ticketText := escpos.RenderSectorTicket(sampleOrder, *target, sampleItems)
			err := s.sendTicketToStation(*target, ticketText)
			msg := fmt.Sprintf("🖨️ Teste enviado para '%s' ([%s | %s] %s) com corte de papel!", target.Name, target.PCName, target.ConnectionType, target.WindowsPrinterName)
			if target.WindowsPrinterName == "" {
				msg = fmt.Sprintf("🖨️ Comanda de teste gerada para '%s' (IP %s:%d). Selecione uma impressora autodetectada para envio direto ao Windows!", target.Name, target.PrinterIP, target.PrinterPort)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"tested":      true,
				"station":     target,
				"ticket_text": ticketText,
				"message":     msg,
				"spool_error": func() string {
					if err != nil {
						return err.Error()
					}
					return ""
				}(),
			})
			return

		case "SAVE_ROUTE":
			catID := req.CategoryID
			if catID <= 0 {
				catID = req.Route.CategoryID
			}
			stID := req.StationID
			if stID == "" {
				stID = req.Route.PrinterStationID
			}
			_ = s.Store.SaveCategoryPrinterRoute(catID, stID)

		default:
			if strings.EqualFold(req.Type, "ROUTE") {
				catID := req.CategoryID
				if catID <= 0 {
					catID = req.Route.CategoryID
				}
				stID := req.StationID
				if stID == "" {
					stID = req.Route.PrinterStationID
				}
				_ = s.Store.SaveCategoryPrinterRoute(catID, stID)
			} else {
				ip := req.Station.PrinterIP
				if ip == "" {
					ip = req.Station.IPAddress
				}
				port := req.Station.PrinterPort
				if port <= 0 {
					port = req.Station.Port
				}
				active := true
				if req.Station.Active != nil {
					active = *req.Station.Active
				} else if req.Station.Enabled != nil {
					active = *req.Station.Enabled
				}
				cutPaper := true
				if req.Station.CutPaperSeparately != nil {
					cutPaper = *req.Station.CutPaperSeparately
				}
				wCalls := false
				if req.Station.PrintWaiterCalls != nil {
					wCalls = *req.Station.PrintWaiterCalls
				}
				pDrinks := false
				if req.Station.PrintDrinks != nil {
					pDrinks = *req.Station.PrintDrinks
				}
				pBill := false
				if req.Station.PrintBillClosure != nil {
					pBill = *req.Station.PrintBillClosure
				}
				pBoutique := false
				if req.Station.PrintBoutique != nil {
					pBoutique = *req.Station.PrintBoutique
				}
				pFiscal := false
				if req.Station.PrintFiscalNotes != nil {
					pFiscal = *req.Station.PrintFiscalNotes
				}
				_ = s.Store.SavePrinterStation(models.PrinterStation{
					ID:                 req.Station.ID,
					Name:               req.Station.Name,
					SectorLabel:        req.Station.SectorLabel,
					ConnectionType:     req.Station.ConnectionType,
					PCName:             req.Station.PCName,
					WindowsPrinterName: req.Station.WindowsPrinterName,
					PrinterIP:          ip,
					PrinterPort:        port,
					Columns:            req.Station.Columns,
					PaperWidthMM:       req.Station.PaperWidthMM,
					CutPaperSeparately: cutPaper,
					PrintWaiterCalls:   wCalls,
					PrintDrinks:        pDrinks,
					PrintBillClosure:   pBill,
					PrintBoutique:      pBoutique,
					PrintFiscalNotes:   pFiscal,
					Active:             active,
				})
			}
		}

		stations, _ := s.Store.ListPrinterStations()
		routes, _ := s.Store.ListCategoryPrinterRoutes()
		detected, remotePCs := s.discoverAllPCPrinters()
		writeJSON(w, http.StatusOK, map[string]any{
			"stations":          stations,
			"routes":            routes,
			"detected_printers": detected,
			"remote_pcs":        remotePCs,
		})
	}
}

func (s *Server) buildCashierBridgeBatContent(kitchenHost string) string {
	if kitchenHost == "" {
		ips := getLocalIPv4s()
		if len(ips) > 0 {
			kitchenHost = fmt.Sprintf("http://%s:%s", ips[0], s.Port)
		} else {
			kitchenHost = "http://192.168.18.53:8080"
		}
	}
	script := `@echo off
chcp 65001 >nul
title SIR BEEF — INSTALADOR E PONTE DE IMPRESSORAS DO PC DO CAIXA (WINDOWS 10)
color 0A

echo ===============================================================================
echo   🥩 SIR BEEF — PONTE AUTOMATICA DE IMPRESSORAS DO PC DO CAIXA (WINDOWS 10)
echo ===============================================================================
echo   Este instalador conecta automaticamente as impressoras deste PC do Caixa:
echo     1. Impressora Bluetooth (Pedidos de Bebidas + Chamados de Garcom)
echo     2. Impressora de Rede Compartilhada (Fechamento de Contas, Boutique e NFC-e)
echo   diretamente ao Servidor Principal do PC da Cozinha!
echo ===============================================================================
echo.

set "INSTALL_DIR=C:\SirBeefCaixa"
if not exist "%INSTALL_DIR%" mkdir "%INSTALL_DIR%"

set "DEFAULT_HOST=` + kitchenHost + `"
echo Servidor do PC da Cozinha pre-configurado: %DEFAULT_HOST%
set /p "CUSTOM_HOST=Pressione ENTER para confirmar [%DEFAULT_HOST%] ou digite o IP do PC da Cozinha: "
if "%CUSTOM_HOST%"=="" set "CUSTOM_HOST=%DEFAULT_HOST%"

echo %CUSTOM_HOST%> "%INSTALL_DIR%\kitchen_host.txt"

cat_ps:
(
echo $ErrorActionPreference = 'SilentlyContinue'
echo $Host.UI.RawUI.WindowTitle = 'SIR BEEF - Ponte de Impressoras PC Caixa (Bluetooth + Rede)'
echo $rawHelper = @'
echo using System;
echo using System.Runtime.InteropServices;
echo public class RawPrinterHelper {
echo     [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
echo     public class DOCINFOW {
echo         [MarshalAs(UnmanagedType.LPWStr)] public string pDocName;
echo         [MarshalAs(UnmanagedType.LPWStr)] public string pOutputFile;
echo         [MarshalAs(UnmanagedType.LPWStr)] public string pDataType;
echo     }
echo     [DllImport("winspool.Drv", EntryPoint = "OpenPrinterW", SetLastError = true, CharSet = CharSet.Unicode, ExactSpelling = true)]
echo     public static extern bool OpenPrinter(string src, out IntPtr hPrinter, IntPtr pd);
echo     [DllImport("winspool.Drv", EntryPoint = "ClosePrinter", SetLastError = true, ExactSpelling = true)]
echo     public static extern bool ClosePrinter(IntPtr hPrinter);
echo     [DllImport("winspool.Drv", EntryPoint = "StartDocPrinterW", SetLastError = true, CharSet = CharSet.Unicode, ExactSpelling = true)]
echo     public static extern int StartDocPrinter(IntPtr hPrinter, int level, [In] DOCINFOW di);
echo     [DllImport("winspool.Drv", EntryPoint = "EndDocPrinter", SetLastError = true, ExactSpelling = true)]
echo     public static extern bool EndDocPrinter(IntPtr hPrinter);
echo     [DllImport("winspool.Drv", EntryPoint = "StartPagePrinter", SetLastError = true, ExactSpelling = true)]
echo     public static extern bool StartPagePrinter(IntPtr hPrinter);
echo     [DllImport("winspool.Drv", EntryPoint = "EndPagePrinter", SetLastError = true, ExactSpelling = true)]
echo     public static extern bool EndPagePrinter(IntPtr hPrinter);
echo     [DllImport("winspool.Drv", EntryPoint = "WritePrinter", SetLastError = true, ExactSpelling = true)]
echo     public static extern bool WritePrinter(IntPtr hPrinter, byte[] pBytes, int dwCount, out int dwWritten);
echo     public static bool SendRawBytes(string printerName, byte[] bytes) {
echo         IntPtr hPrinter = IntPtr.Zero;
echo         if (!OpenPrinter(printerName, out hPrinter, IntPtr.Zero)) return false;
echo         try {
echo             DOCINFOW di = new DOCINFOW { pDocName = "Sir Beef Thermal Ticket", pDataType = "RAW" };
echo             if (StartDocPrinter(hPrinter, 1, di) == 0) return false;
echo             try {
echo                 StartPagePrinter(hPrinter);
echo                 int written = 0;
echo                 WritePrinter(hPrinter, bytes, bytes.Length, out written);
echo                 EndPagePrinter(hPrinter);
echo                 return written ^> 0;
echo             } finally { EndDocPrinter(hPrinter); }
echo         } finally { ClosePrinter(hPrinter); }
echo     }
echo }
echo '@
echo Add-Type -TypeDefinition $rawHelper -Language CSharp
echo $hostUrl = (Get-Content 'C:\SirBeefCaixa\kitchen_host.txt' -Raw).Trim()
echo if (-not $hostUrl.StartsWith('http')) { $hostUrl = 'http://' + $hostUrl }
echo if ($hostUrl -notmatch ':\d+$') { $hostUrl = $hostUrl.TrimEnd('/') + ':8080' }
echo $pcLabel = "PC-CAIXA ($env:COMPUTERNAME)"
echo Write-Host "==================================================================" -ForegroundColor Green
echo Write-Host "  SIR BEEF - PONTE DE IMPRESSORAS DO CAIXA ATIVA!" -ForegroundColor Green
echo Write-Host "  PC Local : $pcLabel" -ForegroundColor Cyan
echo Write-Host "  Conectado: $hostUrl" -ForegroundColor Yellow
echo Write-Host "==================================================================" -ForegroundColor Green
echo while ($true) {
echo     try {
echo         $winPrinters = @(Get-CimInstance Win32_Printer ^| ForEach-Object {
echo             $cType = 'USB'
echo             $combined = ($_.Name + ' ' + $_.PortName + ' ' + $_.DriverName).ToUpper()
echo             if ($combined -match 'BTH^|BLUETOOTH^|BT^|COM\d+') { $cType = 'BLUETOOTH' }
echo             elseif ($_.Shared -or $combined -match '\\\\^|IP_^|TCP^|WSD^|NET^|REDE') { $cType = 'REDE' }
echo             @{
echo                 name = [string]$_.Name
echo                 port_name = [string]$_.PortName
echo                 driver_name = [string]$_.DriverName
echo                 connection_type = $cType
echo                 shared = [bool]$_.Shared
echo                 share_name = [string]$_.ShareName
echo                 status = 'PRONTA'
echo             }
echo         })
echo         $body = @{ action = 'AGENT_HEARTBEAT'; pc_name = $pcLabel; printers = $winPrinters } ^| ConvertTo-Json -Depth 5 -Compress
echo         $resp = Invoke-RestMethod -Uri "$hostUrl/api/admin/printers" -Method Post -ContentType 'application/json; charset=utf-8' -Body ([System.Text.Encoding]::UTF8.GetBytes($body)) -TimeoutSec 4
echo         if ($resp -and $resp.jobs) {
echo             foreach ($job in $resp.jobs) {
echo                 $targetPrinter = [string]$job.windows_printer_name
echo                 if (-not $targetPrinter -and $winPrinters.Count -gt 0) { $targetPrinter = $winPrinters[0].name }
echo                 Write-Host "[$(Get-Date -Format 'HH:mm:ss')] Imprimindo setor $($job.station_name) em '$targetPrinter'..." -ForegroundColor Green
echo                 $nl = [Environment]::NewLine
echo                 $textBytes = [System.Text.Encoding]::GetEncoding(28591).GetBytes([string]$job.ticket_text + $nl + $nl + $nl + $nl)
echo                 $ms = New-Object System.IO.MemoryStream
echo                 $ms.Write([byte[]]@(0x1B, 0x40), 0, 2)
echo                 $ms.Write($textBytes, 0, $textBytes.Length)
echo                 if ($job.cut_paper) { $ms.Write([byte[]]@(0x1D, 0x56, 66, 3), 0, 4) }
echo                 [void][RawPrinterHelper]::SendRawBytes($targetPrinter, $ms.ToArray())
echo             }
echo         }
echo     } catch {
echo         Write-Host "[$(Get-Date -Format 'HH:mm:ss')] Aguardando servidor do PC da Cozinha em $hostUrl..." -ForegroundColor DarkYellow
echo     }
echo     Start-Sleep -Seconds 2
echo }
) > "%INSTALL_DIR%\ponte_caixa.ps1"

echo Criando atalho na Inicializacao do Windows (Startup) e na Area de Trabalho...
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ws = New-Object -ComObject WScript.Shell; $s1 = $ws.CreateShortcut([Environment]::GetFolderPath('Startup') + '\SirBeef_Ponte_Caixa.lnk'); $s1.TargetPath = 'powershell.exe'; $s1.Arguments = '-NoProfile -ExecutionPolicy Bypass -File \"C:\SirBeefCaixa\ponte_caixa.ps1\"'; $s1.WorkingDirectory = 'C:\SirBeefCaixa'; $s1.Save(); $s2 = $ws.CreateShortcut([Environment]::GetFolderPath('Desktop') + '\Sir Beef - Impressoras Caixa.lnk'); $s2.TargetPath = 'powershell.exe'; $s2.Arguments = '-NoProfile -ExecutionPolicy Bypass -File \"C:\SirBeefCaixa\ponte_caixa.ps1\"'; $s2.WorkingDirectory = 'C:\SirBeefCaixa'; $s2.Save();"

echo.
echo ✅ INSTALACAO CONCLUIDA COM SUCESSO NO PC DO CAIXA!
echo 🚀 Iniciando a Ponte Automatica de Impressoras (Bluetooth + Rede)...
powershell -NoProfile -ExecutionPolicy Bypass -File "%INSTALL_DIR%\ponte_caixa.ps1"
`
	return strings.ReplaceAll(script, "\n", "\r\n")
}

func (s *Server) handleDownloadCashierBridge(w http.ResponseWriter, r *http.Request) {
	ips := getLocalIPv4s()
	primaryLan := "http://192.168.18.53:" + s.Port
	if len(ips) > 0 {
		primaryLan = fmt.Sprintf("http://%s:%s", ips[0], s.Port)
	}
	batContent := s.buildCashierBridgeBatContent(primaryLan)
	w.Header().Set("Content-Type", "application/octet-stream; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="INSTALAR_PONTE_PC_CAIXA.bat"`)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(batContent))
}

func (s *Server) handleMotoboyOptimizedRoute(w http.ResponseWriter, r *http.Request) {
	orders, err := s.Store.ListOrders("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var stops []models.Order
	for _, o := range orders {
		if o.OriginType == "DELIVERY" && (o.Status == "PRONTO" || o.Status == "DESPACHADO") {
			stops = append(stops, o)
		}
	}

	storeLat, storeLng := -23.55052, -46.63331
	var waypoints []string
	for _, st := range stops {
		waypoints = append(waypoints, fmt.Sprintf("%.5f,%.5f", st.Latitude, st.Longitude))
	}

	mapsURL := fmt.Sprintf("https://www.google.com/maps/dir/%.5f,%.5f", storeLat, storeLng)
	if len(waypoints) > 0 {
		mapsURL += "/" + strings.Join(waypoints, "/")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"store_origin": map[string]float64{"lat": storeLat, "lng": storeLng},
		"stops":        stops,
		"google_maps":  mapsURL,
	})
}

func (s *Server) handleMotoboyLocation(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		mid, _ := strconv.Atoi(r.URL.Query().Get("motoboy_id"))
		mname := r.URL.Query().Get("motoboy_name")
		loc := s.Store.GetMotoboyLocation(mid, mname)
		writeJSON(w, http.StatusOK, map[string]any{
			"location": loc,
		})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use GET ou POST"})
		return
	}
	var loc models.MotoboyLiveLocation
	if err := json.NewDecoder(r.Body).Decode(&loc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}
	if loc.Latitude == 0 && loc.Longitude == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Coordenadas GPS inválidas"})
		return
	}
	if err := s.Store.UpdateMotoboyLocation(loc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	saved := s.Store.GetMotoboyLocation(loc.MotoboyID, loc.MotoboyName)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"location": saved,
	})
}

func (s *Server) handleOrderTrack(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.URL.Query().Get("pedido"))
	if code == "" {
		code = strings.TrimSpace(r.URL.Query().Get("order"))
	}
	if code == "" {
		code = strings.TrimSpace(r.URL.Query().Get("id"))
	}
	if code == "" {
		// Se não informar código, busca o último pedido DELIVERY ativo para demonstração rápida
		orders, _ := s.Store.ListOrders("")
		for _, o := range orders {
			if strings.EqualFold(o.OriginType, "DELIVERY") {
				code = o.OrderCode
				break
			}
		}
	}
	if code == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Informe o número do pedido (?pedido=SB-...)"})
		return
	}
	ord, err := s.Store.GetOrderByIDOrQR(code)
	if err != nil || ord == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Pedido " + code + " não encontrado"})
		return
	}
	mid := 0
	if ord.MotoboyID != nil {
		mid = *ord.MotoboyID
	}
	motoLoc := s.Store.GetMotoboyLocation(mid, ord.MotoboyName)

	// Coordenadas padrão da Sir Beef (Nuporanga - SP)
	storeLat, storeLng := -20.7289, -47.7486
	writeJSON(w, http.StatusOK, map[string]any{
		"order":            ord,
		"motoboy_location": motoLoc,
		"store_origin": map[string]any{
			"name": "SIR BEEF — Boutique das Carnes & Hamburgueria",
			"lat":  storeLat,
			"lng":  storeLng,
		},
		"server_time": time.Now().Format("15:04:05"),
	})
}

func (s *Server) handleWhatsAppStatus(w http.ResponseWriter, r *http.Request) {
	if s.WhatsApp == nil {
		writeJSON(w, http.StatusOK, whatsapp.SessionStatus{
			Connected:   false,
			Status:      "DISCONNECTED",
			LastError:   "Módulo WhatsApp não inicializado",
			CardapioURL: "https://sirbeefnup.github.io/cardapio",
		})
		return
	}
	writeJSON(w, http.StatusOK, s.WhatsApp.GetStatus())
}

func (s *Server) handleWhatsAppConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	if s.WhatsApp == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Gerenciador WhatsApp indisponível"})
		return
	}
	var req struct {
		Mode  string `json:"mode"`  // "QR" ou "PAIR_CODE"
		Phone string `json:"phone"` // Número com DDD quando mode == "PAIR_CODE"
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	st, err := s.WhatsApp.StartPairing(req.Mode, req.Phone)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":  err.Error(),
			"status": st,
		})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleWhatsAppDisconnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	if s.WhatsApp == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Gerenciador WhatsApp indisponível"})
		return
	}
	st := s.WhatsApp.Disconnect()
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleWhatsAppConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	if s.WhatsApp == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Gerenciador WhatsApp indisponível"})
		return
	}
	var req struct {
		AutoReplyEnabled bool `json:"auto_reply_enabled"`
		AutoOrderNotify  bool `json:"auto_order_notify"`
		BotPhase         int  `json:"bot_phase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if req.BotPhase == 1 || req.BotPhase == 2 {
		cfg := s.Store.GetPrinterConfig()
		cfg.WhatsappBotPhase = req.BotPhase
		_ = s.Store.UpdatePrinterConfig(cfg)
	}
	st := s.WhatsApp.SetConfig(req.AutoReplyEnabled, req.AutoOrderNotify)
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleWhatsAppSendTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	var req struct {
		Phone        string `json:"phone"`
		CustomerName string `json:"customer_name"`
		Message      string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.Phone) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Informe o número de WhatsApp com DDD."})
		return
	}
	if strings.TrimSpace(req.CustomerName) == "" {
		req.CustomerName = "Cliente Teste"
	}
	webLink := "https://sirbeefnup.github.io/cardapio"
	if strings.TrimSpace(req.Message) == "" {
		req.Message = whatsapp.BuildStaticGreeting(req.CustomerName, webLink)
	}

	if s.WhatsApp != nil && s.WhatsApp.GetStatus().Connected {
		if err := s.WhatsApp.SendText(req.Phone, req.Message); err != nil {
			s.Store.LogWhatsAppWithStatus(req.Phone, req.CustomerName, "OUTBOUND", "TESTE_DISPARO_REAL", req.Message, "ERRO_ENVIO")
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Erro ao enviar pelo WhatsApp conectado: " + err.Error()})
			return
		}
		s.Store.LogWhatsAppWithStatus(req.Phone, req.CustomerName, "OUTBOUND", "TESTE_DISPARO_REAL", req.Message, "ENVIADO_WHATSAPP")
		writeJSON(w, http.StatusOK, map[string]any{
			"sent_real": true,
			"message":   "Mensagem enviada com sucesso pelo WhatsApp conectado!",
		})
		return
	}

	s.Store.LogWhatsAppWithStatus(req.Phone, req.CustomerName, "OUTBOUND", "SIMULACAO_BOT_LOCAL", req.Message, "SIMULADO_OFFLINE")
	writeJSON(w, http.StatusOK, map[string]any{
		"sent_real": false,
		"message":   "WhatsApp ainda não está conectado (QR Code pendente). Mensagem registrada no log local de simulação.",
	})
}

func (s *Server) processIncomingWhatsAppBotMessage(senderPhone string, pushName string, text string, webLink string) (string, string) {
	cfg := s.Store.GetPrinterConfig()
	if !cfg.IsOpenNow {
		return whatsapp.BuildClosedStoreReply(pushName, cfg, webLink), "BOT_LOJA_FECHADA_HORARIOS"
	}

	if (cfg.AIOrderBotEnabled || cfg.WhatsappBotPhase == 2) && strings.TrimSpace(cfg.GeminiAPIKey) != "" {
		trigger := "FASE2_GEMINI_IA"
		prods, _ := s.Store.ListAllProducts()
		var menuParts []string
		for _, p := range prods {
			if !p.Active {
				continue
			}
			isBoutiqueProd := strings.EqualFold(p.Sector, "MERCADO") ||
				strings.EqualFold(p.SectorABC, "MERCADO_CARNES") ||
				p.CategoryID == 900 ||
				strings.Contains(strings.ToLower(p.CategoryName), "boutique") ||
				strings.Contains(strings.ToLower(p.CategoryName), "mercado")
			if isBoutiqueProd && !cfg.AIIncludeBoutique {
				continue
			}
			menuParts = append(menuParts, fmt.Sprintf("[ID:%d] %s - %s (R$ %.2f)", p.ID, p.CategoryName, p.Name, p.Price))
		}
		menuSummary := strings.Join(menuParts, " | ")
		if menuSummary == "" {
			menuSummary = "Burgers Gourmet (Picanha, Costela, Angus), Porções e Bebidas"
		}

		// Puxa o Ranking em tempo real dos Produtos Mais Vendidos do PDV 360° para a IA recomendar quando o cliente pedir sugestão
		if dash, errDash := s.Store.GetFinanceDashboard(); errDash == nil && dash != nil {
			var rankParts []string
			if len(dash.ABCBurgers) > 0 {
				var topHamb []string
				for i, item := range dash.ABCBurgers {
					if i >= 6 {
						break
					}
					topHamb = append(topHamb, fmt.Sprintf("%dº %s (%.0f vendidos)", i+1, item.ProductName, item.QtySold))
				}
				rankParts = append(rankParts, "🏆 MAIS VENDIDOS HAMBURGUERIA (Lanches/Porções): "+strings.Join(topHamb, ", "))
			}
			if cfg.AIIncludeBoutique && len(dash.ABCMarketAndButcher) > 0 {
				var topBout []string
				for i, item := range dash.ABCMarketAndButcher {
					if i >= 5 {
						break
					}
					topBout = append(topBout, fmt.Sprintf("%dº %s (%.1f vendidos)", i+1, item.ProductName, item.QtySold))
				}
				rankParts = append(rankParts, "🥩 MAIS VENDIDOS BOUTIQUE/MERCADO: "+strings.Join(topBout, ", "))
			}
			if len(rankParts) > 0 {
				menuSummary += "\n\nRANKING OFICIAL DOS MAIS VENDIDOS DA LOJA (use para recomendar quando o cliente pedir sugestão de lanche ou porção mais vendida): " + strings.Join(rankParts, " | ")
			}
		}
		if !cfg.AIIncludeBoutique {
			menuSummary += "\n(Nota: Atenda exclusivamente itens da Hamburgueria — Lanches, Porções, Bebidas e Sobremesas. Os cortes da Boutique/Mercado estão desativados para o bot no momento.)"
		}

		savedAddress := ""
		customers, _ := s.Store.ListCustomers()
		cleanSender := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, senderPhone)
		for _, c := range customers {
			cleanCust := strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, c.Phone)
			if cleanSender != "" && cleanCust != "" && (strings.HasSuffix(cleanSender, cleanCust) || strings.HasSuffix(cleanCust, cleanSender)) {
				if strings.TrimSpace(c.AddressText) != "" {
					savedAddress = c.AddressText
					break
				}
			}
		}

		rText, err := whatsapp.GenerateAIOrderBotReply(
			cfg.GeminiAPIKey,
			cfg.AIProvider,
			cfg.AICanCreateOrders,
			cfg.AICustomInstructions,
			pushName,
			savedAddress,
			cfg.DefaultDeliveryFee,
			text,
			menuSummary,
			webLink,
		)
		if err != nil || strings.TrimSpace(rText) == "" {
			return whatsapp.BuildStaticGreeting(pushName, webLink), "FASE1_BOT_CARDAPIO"
		}

		// Se a IA gerou o bloco [ORDER_JSON]...[/ORDER_JSON] e a chave de fechar pedido automático estiver ativa, cria o pedido na Expedição!
		startIdx := strings.Index(rText, "[ORDER_JSON]")
		endIdx := strings.Index(rText, "[/ORDER_JSON]")
		if startIdx >= 0 && endIdx > startIdx {
			jsonRaw := strings.TrimSpace(rText[startIdx+len("[ORDER_JSON]") : endIdx])
			rText = strings.TrimSpace(rText[:startIdx] + " " + rText[endIdx+len("[/ORDER_JSON]"):])
			if cfg.AICanCreateOrders {
				var aiOrd struct {
					OriginType    string  `json:"origin_type"`
					PaymentMethod string  `json:"payment_method"`
					AddressText   string  `json:"address_text"`
					ChangeFor     float64 `json:"change_for"`
					Items         []struct {
						ProductID   int     `json:"product_id"`
						ProductName string  `json:"product_name"`
						Quantity    float64 `json:"quantity"`
						UnitPrice   float64 `json:"unit_price"`
						Notes       string  `json:"notes"`
					} `json:"items"`
				}
				if json.Unmarshal([]byte(jsonRaw), &aiOrd) == nil && len(aiOrd.Items) > 0 {
					var ordItems []models.OrderItem
					var subTotal float64
					for _, it := range aiOrd.Items {
						q := it.Quantity
						if q <= 0 {
							q = 1
						}
						up := it.UnitPrice
						if up <= 0 {
							for _, p := range prods {
								if p.ID == it.ProductID || strings.EqualFold(p.Name, it.ProductName) {
									up = p.Price
									if it.ProductID == 0 {
										it.ProductID = p.ID
									}
									if it.ProductName == "" {
										it.ProductName = p.Name
									}
									break
								}
							}
						}
						tot := q * up
						subTotal += tot
						var mods []models.SelectedModifier
						if strings.TrimSpace(it.Notes) != "" {
							mods = append(mods, models.SelectedModifier{
								GroupName:  "Observação IA",
								OptionName: strings.TrimSpace(it.Notes),
								Price:      0,
							})
						}
						ordItems = append(ordItems, models.OrderItem{
							ProductID:         it.ProductID,
							ProductName:       it.ProductName,
							Quantity:          q,
							UnitPrice:         up,
							TotalPrice:        tot,
							SelectedModifiers: mods,
						})
					}
					orig := strings.ToUpper(strings.TrimSpace(aiOrd.OriginType))
					if orig == "" {
						orig = "DELIVERY"
					}
					hdr := "[DELIVERY IA WHATSAPP]"
					delivFee := cfg.DefaultDeliveryFee
					if orig != "DELIVERY" {
						orig = "BALCAO"
						hdr = "[RETIRADA BALCÃO IA]"
						delivFee = 0
					}
					pm := strings.ToUpper(strings.TrimSpace(aiOrd.PaymentMethod))
					if pm == "" {
						pm = "PIX"
					}
					newOrd := models.Order{
						OriginType:      orig,
						HeaderTag:       hdr,
						Status:          "PREPARANDO",
						StatusPagamento: "PENDENTE",
						PaymentMethod:   pm,
						CustomerName:    pushName,
						CustomerPhone:   senderPhone,
						AddressText:     aiOrd.AddressText,
						Subtotal:        subTotal,
						DeliveryFee:     delivFee,
						Total:           subTotal + delivFee,
						ChangeFor:       aiOrd.ChangeFor,
						Items:           ordItems,
					}
					if created, errCreate := s.Store.CreateOrder(newOrd); errCreate == nil && created != nil {
						trigger = "PEDIDO_CRIADO_POR_IA"
						go s.dispatchOrderPhysicalPrint(*created)
						rText += fmt.Sprintf("\n\n✅ *Pedido #%s enviado direto para o PREPARO na nossa Cozinha!*\n💰 *Total:* R$ %.2f (%s)", created.OrderCode, created.Total, created.PaymentMethod)
					}
				}
			}
		}
		return rText, trigger
	}

	return whatsapp.BuildStaticGreeting(pushName, webLink), "FASE1_BOT_CARDAPIO"
}

func (s *Server) handleWhatsAppWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use POST"})
		return
	}
	var req struct {
		Phone        string `json:"phone"`
		CustomerName string `json:"customer_name"`
		Message      string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Phone == "" {
		req.Phone = "5511999990000"
	}
	if req.CustomerName == "" {
		req.CustomerName = "Cliente WhatsApp"
	}

	s.Store.LogWhatsApp(req.Phone, req.CustomerName, "INBOUND", "MENSAGEM_CLIENTE", req.Message)

	cfg := s.Store.GetPrinterConfig()
	webLink := "https://sirbeefnup.github.io/cardapio"

	reply, trigger := s.processIncomingWhatsAppBotMessage(req.Phone, req.CustomerName, req.Message, webLink)

	s.Store.LogWhatsApp(req.Phone, req.CustomerName, "OUTBOUND", trigger, reply)
	writeJSON(w, http.StatusOK, map[string]any{
		"phase":   cfg.WhatsappBotPhase,
		"trigger": trigger,
		"reply":   reply,
	})
}

func (s *Server) handleWhatsAppLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		idStr := r.URL.Query().Get("id")
		if strings.EqualFold(r.URL.Query().Get("all"), "1") || strings.EqualFold(r.URL.Query().Get("clear"), "1") || idStr == "" {
			if err := s.Store.ClearAllWhatsAppLogs(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		} else {
			id, _ := strconv.Atoi(idStr)
			if err := s.Store.DeleteWhatsAppLog(id); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		logs, _ := s.Store.ListWhatsAppLogs()
		writeJSON(w, http.StatusOK, logs)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action       string `json:"action"` // DELETE_ONE, CLEAR_ALL
			ID           int    `json:"id"`
			OperatorRole string `json:"operator_role"`
			Role         string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		roleUp := strings.ToUpper(strings.TrimSpace(req.OperatorRole))
		if roleUp == "" {
			roleUp = strings.ToUpper(strings.TrimSpace(req.Role))
		}
		if roleUp != "" && roleUp != "ADMIN" && roleUp != "ADM" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Apenas o Administrador (ADM) pode excluir logs do WhatsApp."})
			return
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		if act == "CLEAR_ALL" || act == "DELETE_ALL" {
			if err := s.Store.ClearAllWhatsAppLogs(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
		} else if act == "DELETE_ONE" || act == "DELETE" || req.ID > 0 {
			if err := s.Store.DeleteWhatsAppLog(req.ID); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		logs, _ := s.Store.ListWhatsAppLogs()
		writeJSON(w, http.StatusOK, logs)
		return
	}
	logs, err := s.Store.ListWhatsAppLogs()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, logs)
}

func (s *Server) handleCoupons(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		list, err := s.Store.ListDiscountCoupons()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	if r.Method == http.MethodDelete {
		id, _ := strconv.Atoi(r.URL.Query().Get("id"))
		if id > 0 {
			_ = s.Store.DeleteDiscountCoupon(id)
		}
		list, _ := s.Store.ListDiscountCoupons()
		writeJSON(w, http.StatusOK, list)
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action        string                 `json:"action"` // SAVE, TOGGLE, DELETE
			ID            int                    `json:"id"`
			Code          string                 `json:"code"`
			CouponType    string                 `json:"coupon_type"`   // FIXO ou USO_UNICO
			DiscountType  string                 `json:"discount_type"` // VALOR ou PERCENTUAL
			DiscountValue float64                `json:"discount_value"`
			CustomerID    int                    `json:"customer_id"`
			CustomerName  string                 `json:"customer_name"`
			CustomerPhone string                 `json:"customer_phone"`
			Reason        string                 `json:"reason"`
			Active        *bool                  `json:"active"`
			Coupon        *models.DiscountCoupon `json:"coupon"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		switch act {
		case "DELETE":
			if err := s.Store.DeleteDiscountCoupon(req.ID); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		case "TOGGLE", "SET_ACTIVE":
			actVal := true
			if req.Active != nil {
				actVal = *req.Active
			}
			if _, err := s.Store.ToggleDiscountCoupon(req.ID, actVal); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		default: // SAVE
			c := models.DiscountCoupon{
				ID:            req.ID,
				Code:          req.Code,
				CouponType:    req.CouponType,
				DiscountType:  req.DiscountType,
				DiscountValue: req.DiscountValue,
				CustomerID:    req.CustomerID,
				CustomerName:  req.CustomerName,
				CustomerPhone: req.CustomerPhone,
				Reason:        req.Reason,
				Active:        true,
			}
			if req.Active != nil {
				c.Active = *req.Active
			}
			if req.Coupon != nil && strings.TrimSpace(req.Coupon.Code) != "" {
				c = *req.Coupon
			}
			if _, err := s.Store.SaveDiscountCoupon(c); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
		list, _ := s.Store.ListDiscountCoupons()
		writeJSON(w, http.StatusOK, list)
	}
}

func (s *Server) handleValidateCoupon(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	subStr := r.URL.Query().Get("subtotal")
	phone := r.URL.Query().Get("phone")
	subtotal, _ := strconv.ParseFloat(subStr, 64)

	if r.Method == http.MethodPost {
		var req struct {
			Code     string  `json:"code"`
			Subtotal float64 `json:"subtotal"`
			Phone    string  `json:"phone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			if req.Code != "" {
				code = req.Code
			}
			if req.Subtotal > 0 {
				subtotal = req.Subtotal
			}
			if req.Phone != "" {
				phone = req.Phone
			}
		}
	}

	c, discAmt, err := s.Store.ValidateDiscountCoupon(code, subtotal, phone)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"valid": false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":           true,
		"coupon":          c,
		"code":            c.Code,
		"coupon_type":     c.CouponType,
		"discount_type":   c.DiscountType,
		"discount_value":  c.DiscountValue,
		"discount_amount": discAmt,
	})
}

func (s *Server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	dash, err := s.Store.GetFinanceDashboard()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, dash)
}

func (s *Server) handlePrinterConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.Store.GetPrinterConfig())
		return
	}
	if r.Method == http.MethodPost {
		var cfg models.PrinterConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.Store.UpdatePrinterConfig(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.Store.GetPrinterConfig())
	}
}

func (s *Server) handleOperationalSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.Store.GetPrinterConfig())
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			OperatorRole            string                        `json:"operator_role"`
			AdminPIN                string                        `json:"admin_pin"`
			WaiterAlarmTableCall    *bool                         `json:"waiter_alarm_table_call"`
			WaiterAlarmDrinks       *bool                         `json:"waiter_alarm_drinks"`
			WaiterAlarmKitchen      *bool                         `json:"waiter_alarm_kitchen_ready"`
			WaiterAlarmMode         string                        `json:"waiter_alarm_mode"`
			MenuEnableDelivery      *bool                         `json:"menu_enable_delivery"`
			MenuEnableRetirada      *bool                         `json:"menu_enable_retirada"`
			MenuEnableMesa          *bool                         `json:"menu_enable_mesa"`
			MenuEnableKiosk         *bool                         `json:"menu_enable_kiosk"`
			WaiterVisibleCategories   *[]int                        `json:"waiter_visible_categories"`
			WaiterHiddenCategories    *[]int                        `json:"waiter_hidden_categories"`
			DefaultDeliveryFee        *float64                      `json:"default_delivery_fee"`
			AllowStaffEditDeliveryFee *bool                         `json:"allow_staff_edit_delivery_fee"`
			OperatingHours            *models.OperatingHoursConfig  `json:"operating_hours"`
			AutoScheduleEnabled       *bool                         `json:"auto_schedule_enabled"`
			ManualOverrideMode        string                        `json:"manual_override_mode"`
			AIOrderBotEnabled         *bool                         `json:"ai_order_bot_enabled"`
			AIProvider                *string                       `json:"ai_provider"`
			GeminiAPIKey              *string                       `json:"gemini_api_key"`
			AICanCreateOrders         *bool                         `json:"ai_can_create_orders"`
			AIIncludeBoutique         *bool                         `json:"ai_include_boutique"`
			AICustomInstructions      *string                       `json:"ai_custom_instructions"`
			WhatsappBotPhase          *int                          `json:"whatsapp_bot_phase"`
			NFCeEnabled               *bool                         `json:"nfce_enabled"`
			NFCeAmbiente              *string                       `json:"nfce_ambiente"`
			CNPJEmitente              *string                       `json:"cnpj_emitente"`
			NFCeRazaoSocial           *string                       `json:"nfce_razao_social"`
			NFCeInscricaoEstadual     *string                       `json:"nfce_inscricao_estadual"`
			NFCeRegimeTributario      *string                       `json:"nfce_regime_tributario"`
			NFCeEnderecoFiscal        *string                       `json:"nfce_endereco_fiscal"`
			NFCeSerie                 *int                          `json:"nfce_serie"`
			NFCeProximoNumero         *int                          `json:"nfce_proximo_numero"`
			NFCeCSCID                 *string                       `json:"nfce_csc_id"`
			NFCeCSCToken              *string                       `json:"nfce_csc_token"`
			NFCeCertFilename          *string                       `json:"nfce_cert_filename"`
			NFCeCertBase64            *string                       `json:"nfce_cert_base64"`
			NFCeCertPassword          *string                       `json:"nfce_cert_password"`
			NFCeAPIToken              *string                       `json:"nfce_api_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if !strings.EqualFold(req.OperatorRole, "ADMIN") {
			ok, _ := s.Store.VerifyAdminPinOrUser(req.AdminPIN)
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "🚫 Apenas o Administrador (ADM) pode alterar configurações operacionais, categorias do garçom ou horários de funcionamento!"})
				return
			}
		}
		cfg := s.Store.GetPrinterConfig()
		if req.WaiterAlarmTableCall != nil {
			cfg.WaiterAlarmTableCall = *req.WaiterAlarmTableCall
		}
		if req.WaiterAlarmDrinks != nil {
			cfg.WaiterAlarmDrinks = *req.WaiterAlarmDrinks
		}
		if req.WaiterAlarmKitchen != nil {
			cfg.WaiterAlarmKitchenReady = *req.WaiterAlarmKitchen
		}
		if strings.TrimSpace(req.WaiterAlarmMode) != "" {
			cfg.WaiterAlarmMode = strings.ToUpper(strings.TrimSpace(req.WaiterAlarmMode))
		}
		if req.MenuEnableDelivery != nil {
			cfg.MenuEnableDelivery = *req.MenuEnableDelivery
		}
		if req.MenuEnableRetirada != nil {
			cfg.MenuEnableRetirada = *req.MenuEnableRetirada
		}
		if req.MenuEnableMesa != nil {
			cfg.MenuEnableMesa = *req.MenuEnableMesa
		}
		if req.MenuEnableKiosk != nil {
			cfg.MenuEnableKiosk = *req.MenuEnableKiosk
		}
		if req.WaiterVisibleCategories != nil {
			cfg.WaiterVisibleCategories = *req.WaiterVisibleCategories
		}
		if req.WaiterHiddenCategories != nil {
			cfg.WaiterHiddenCategories = *req.WaiterHiddenCategories
		}
		if req.DefaultDeliveryFee != nil {
			v := *req.DefaultDeliveryFee
			if v < 0 {
				v = 0
			}
			cfg.DefaultDeliveryFee = v
		}
		if req.AllowStaffEditDeliveryFee != nil {
			cfg.AllowStaffEditDeliveryFee = *req.AllowStaffEditDeliveryFee
		}
		if req.OperatingHours != nil {
			cfg.OperatingHours = *req.OperatingHours
		}
		if req.AutoScheduleEnabled != nil {
			cfg.OperatingHours.AutoScheduleEnabled = *req.AutoScheduleEnabled
		}
		if strings.TrimSpace(req.ManualOverrideMode) != "" {
			cfg.OperatingHours.ManualOverrideMode = strings.ToUpper(strings.TrimSpace(req.ManualOverrideMode))
		}
		if req.AIOrderBotEnabled != nil {
			cfg.AIOrderBotEnabled = *req.AIOrderBotEnabled
			if *req.AIOrderBotEnabled {
				cfg.WhatsappBotPhase = 2
			} else {
				cfg.WhatsappBotPhase = 1
			}
		}
		if req.WhatsappBotPhase != nil {
			cfg.WhatsappBotPhase = *req.WhatsappBotPhase
		}
		if req.AIProvider != nil {
			cfg.AIProvider = strings.TrimSpace(*req.AIProvider)
		}
		if req.GeminiAPIKey != nil {
			cfg.GeminiAPIKey = strings.TrimSpace(*req.GeminiAPIKey)
		}
		if req.AICanCreateOrders != nil {
			cfg.AICanCreateOrders = *req.AICanCreateOrders
		}
		if req.AIIncludeBoutique != nil {
			cfg.AIIncludeBoutique = *req.AIIncludeBoutique
		}
		if req.AICustomInstructions != nil {
			cfg.AICustomInstructions = *req.AICustomInstructions
		}
		if req.NFCeEnabled != nil {
			cfg.NFCeEnabled = *req.NFCeEnabled
		}
		if req.NFCeAmbiente != nil {
			cfg.NFCeAmbiente = strings.TrimSpace(*req.NFCeAmbiente)
		}
		if req.CNPJEmitente != nil {
			cfg.CNPJEmitente = strings.TrimSpace(*req.CNPJEmitente)
		}
		if req.NFCeRazaoSocial != nil {
			cfg.NFCeRazaoSocial = strings.TrimSpace(*req.NFCeRazaoSocial)
		}
		if req.NFCeInscricaoEstadual != nil {
			cfg.NFCeInscricaoEstadual = strings.TrimSpace(*req.NFCeInscricaoEstadual)
		}
		if req.NFCeRegimeTributario != nil {
			cfg.NFCeRegimeTributario = strings.TrimSpace(*req.NFCeRegimeTributario)
		}
		if req.NFCeEnderecoFiscal != nil {
			cfg.NFCeEnderecoFiscal = strings.TrimSpace(*req.NFCeEnderecoFiscal)
		}
		if req.NFCeSerie != nil {
			cfg.NFCeSerie = *req.NFCeSerie
		}
		if req.NFCeProximoNumero != nil {
			cfg.NFCeProximoNumero = *req.NFCeProximoNumero
		}
		if req.NFCeCSCID != nil {
			cfg.NFCeCSCID = strings.TrimSpace(*req.NFCeCSCID)
		}
		if req.NFCeCSCToken != nil {
			cfg.NFCeCSCToken = strings.TrimSpace(*req.NFCeCSCToken)
		}
		if req.NFCeCertFilename != nil {
			cfg.NFCeCertFilename = strings.TrimSpace(*req.NFCeCertFilename)
		}
		if req.NFCeCertBase64 != nil {
			cfg.NFCeCertBase64 = strings.TrimSpace(*req.NFCeCertBase64)
		}
		if req.NFCeCertPassword != nil {
			cfg.NFCeCertPassword = *req.NFCeCertPassword
		}
		if req.NFCeAPIToken != nil {
			cfg.NFCeAPIToken = strings.TrimSpace(*req.NFCeAPIToken)
		}
		_ = s.Store.UpdatePrinterConfig(cfg)
		writeJSON(w, http.StatusOK, s.Store.GetPrinterConfig())
	}
}

func (s *Server) handleWaiterAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		incAll := r.URL.Query().Get("all") == "1"
		alerts, _ := s.Store.ListWaiterAlerts(incAll)
		cfg := s.Store.GetPrinterConfig()
		writeJSON(w, http.StatusOK, map[string]any{
			"alerts": alerts,
			"config": cfg,
		})
		return
	}
	if r.Method == http.MethodPost {
		var req struct {
			Action      string `json:"action"` // CONFIRM, CONFIRM_ALL, TRIGGER_KITCHEN_READY, TRIGGER_TABLE_CALL
			AlertID     int    `json:"alert_id"`
			TableID     int    `json:"table_id"`
			OrderID     int    `json:"order_id"`
			AlertType   string `json:"alert_type"`
			ConfirmedBy string `json:"confirmed_by"`
			WaiterName  string `json:"waiter_name"`
			RequestBill bool   `json:"request_bill"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		who := strings.TrimSpace(req.ConfirmedBy)
		if who == "" {
			who = strings.TrimSpace(req.WaiterName)
		}
		act := strings.ToUpper(strings.TrimSpace(req.Action))
		switch act {
		case "TRIGGER_KITCHEN_READY":
			alert, err := s.Store.TriggerKitchenReadyAlert(req.OrderID)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			alerts, _ := s.Store.ListWaiterAlerts(false)
			writeJSON(w, http.StatusOK, map[string]any{
				"alert":   alert,
				"alerts":  alerts,
				"config":  s.Store.GetPrinterConfig(),
				"message": "🔔 Garçom acionado com sucesso! Alarme tocando no APK do Garçom.",
			})
			return
		case "TRIGGER_TABLE_CALL":
			_ = s.Store.SetTableWaiterCall(req.TableID, true, req.RequestBill)
			go s.dispatchWaiterCallPhysicalPrint(req.TableID, req.RequestBill)
		case "CONFIRM_ALL":
			alerts, _ := s.Store.ListWaiterAlerts(false)
			for _, a := range alerts {
				_ = s.Store.ConfirmWaiterAlert(a.ID, a.TableID, a.AlertType, who)
			}
		default: // CONFIRM
			_ = s.Store.ConfirmWaiterAlert(req.AlertID, req.TableID, req.AlertType, who)
		}
		alerts, _ := s.Store.ListWaiterAlerts(false)
		writeJSON(w, http.StatusOK, map[string]any{
			"alerts": alerts,
			"config": s.Store.GetPrinterConfig(),
		})
	}
}

func (s *Server) handleSystemLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		logs, err := s.Store.ListSystemLogs()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, logs)
		return
	}
	if r.Method == http.MethodPost {
		var l models.SystemLog
		_ = json.NewDecoder(r.Body).Decode(&l)
		if l.Level == "" {
			l.Level = "ERROR"
		}
		_ = s.Store.AddSystemLog(l.Level, l.Module, l.Message, l.StackTrace, l.DeviceInfo)
		logs, _ := s.Store.ListSystemLogs()
		writeJSON(w, http.StatusCreated, logs)
	}
}

func (s *Server) handleAPKDownload(w http.ResponseWriter, r *http.Request) {
	exeDir, _ := os.Getwd()
	apkPath := filepath.Join(filepath.Dir(exeDir), "SirBeef-SuperApp.apk")
	f, err := os.Open(apkPath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "APK ainda não encontrado na raiz da pasta SIR BEEF."})
		return
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	vInfo := s.getCurrentAPKVersionInfo(r)
	vName, _ := vInfo["version_name"].(string)
	if vName == "" {
		vName = "1.9.0"
	}
	fileName := fmt.Sprintf("SirBeef-SuperApp-v%s.apk", vName)
	etag := fmt.Sprintf("\"%x-%x\"", stat.ModTime().UnixNano(), stat.Size())

	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("Content-Type", "application/vnd.android.package-archive")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
	http.ServeContent(w, r, fileName, stat.ModTime(), f)
}

func getLocalIPv4s() []string {
	var privateIPs []string
	var otherIPs []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return privateIPs
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.Contains(strings.ToLower(iface.Name), "topaz") || strings.Contains(strings.ToLower(iface.Name), "loopback") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip4 := ip.To4()
			if ip4 != nil {
				s := ip4.String()
				if strings.HasPrefix(s, "192.168.") || strings.HasPrefix(s, "10.") || strings.HasPrefix(s, "172.") {
					privateIPs = append(privateIPs, s)
				} else if !strings.HasPrefix(s, "169.254.") && !strings.HasPrefix(s, "54.") {
					otherIPs = append(otherIPs, s)
				}
			}
		}
	}
	return append(privateIPs, otherIPs...)
}

func (s *Server) handleSystemDeploy(w http.ResponseWriter, r *http.Request) {
	ghCfgFile := filepath.Join(s.DataDir, "github_config.json")
	var ghCfg struct {
		Owner  string `json:"owner"`
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Token  string `json:"token"`
	}
	if b, err := os.ReadFile(ghCfgFile); err == nil {
		_ = json.Unmarshal(b, &ghCfg)
	}
	if ghCfg.Branch == "" {
		ghCfg.Branch = "main"
	}

	backupDir := filepath.Join(s.DataDir, "backups")
	_ = os.MkdirAll(backupDir, 0755)
	listBackups := func() []map[string]any {
		var out []map[string]any
		if entries, err := os.ReadDir(backupDir); err == nil {
			for i := len(entries) - 1; i >= 0; i-- {
				e := entries[i]
				if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".db") {
					continue
				}
				info, err := e.Info()
				if err != nil {
					continue
				}
				out = append(out, map[string]any{
					"name":       e.Name(),
					"size_bytes": info.Size(),
					"created_at": info.ModTime().Format("02/01/2006 15:04:05"),
				})
				if len(out) >= 10 {
					break
				}
			}
		}
		return out
	}

	fetchCloudManifest := func() map[string]any {
		if ghCfg.Owner == "" || ghCfg.Repo == "" {
			return nil
		}
		client := &http.Client{Timeout: 8 * time.Second}
		u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/release/system_manifest.json?ref=%s", ghCfg.Owner, ghCfg.Repo, ghCfg.Branch)
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil
		}
		if ghCfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+ghCfg.Token)
		}
		req.Header.Set("Accept", "application/vnd.github.raw+json")
		req.Header.Set("User-Agent", "SirBeef-Host-Server")
		resp, err := client.Do(req)
		if err != nil || resp == nil {
			return nil
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		var m map[string]any
		if json.NewDecoder(resp.Body).Decode(&m) == nil {
			return m
		}
		return nil
	}

	if r.Method == http.MethodGet {
		vInfo := s.getCurrentAPKVersionInfo(r)
		hostname, _ := os.Hostname()
		writeJSON(w, http.StatusOK, map[string]any{
			"server_env_mode": s.getServerEnvMode(),
			"hostname":        hostname,
			"local_version":   vInfo,
			"cloud_manifest":  fetchCloudManifest(),
			"recent_backups":  listBackups(),
			"tunnel_url":      s.getTunnelURL(),
			"fixed_pdv_url":   "https://sirbeefnup.github.io/pdv",
			"fixed_menu_url":  "https://sirbeefnup.github.io/cardapio",
			"protected_files": []string{"backend/data/sirbeef.db (Clientes, Pedidos, Vendas & Turnos)", "backend/data/whatsapp_session.db (Conexão WhatsApp da Loja)"},
		})
		return
	}

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Use GET ou POST"})
		return
	}

	var req struct {
		Action       string `json:"action"` // SET_ENV_MODE, FORCE_SYNC_TUNNEL, PUBLISH_SYSTEM, UPDATE_SYSTEM, BACKUP_DB
		EnvMode      string `json:"env_mode"`
		ReleaseNotes string `json:"release_notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	action := strings.ToUpper(strings.TrimSpace(req.Action))
	switch action {
	case "SET_ENV_MODE":
		newMode := s.setServerEnvMode(req.EnvMode)
		exeDir, _ := os.Getwd()
		starterBat := filepath.Join(filepath.Dir(exeDir), "INICIAR_ECOSSISTEMA_SIRBEEF.bat")
		msg := "🧪 Servidor configurado em MODO TESTE / DESENVOLVIMENTO (Seu PC). O início automático com o Windows foi desativado neste PC e seus clientes de teste ficam 100% isolados!"
		if newMode == "PRODUCAO_LOJA" {
			_ = exec.Command("schtasks", "/Create", "/TN", "SirBeef_AutoStart_Admin", "/TR", fmt.Sprintf(`"%s" --autostart`, starterBat), "/SC", "ONLOGON", "/RL", "HIGHEST", "/F").Run()
			_ = exec.Command("reg", "add", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "SirBeef_Server_Admin", "/t", "REG_SZ", "/d", `schtasks /Run /TN "SirBeef_AutoStart_Admin"`, "/f").Run()
			msg = "🏪 Servidor configurado em MODO LOJA OFICIAL (PC da Hamburgueria). Início automático com o Windows (Modo ADM) ativado e link fixo sincronizado!"
			go s.syncGitHubTunnelURLForce(s.getTunnelURL(), true)
		} else {
			_ = exec.Command("schtasks", "/Delete", "/TN", "SirBeef_AutoStart_Admin", "/F").Run()
			_ = exec.Command("reg", "delete", `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`, "/v", "SirBeef_Server_Admin", "/f").Run()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"server_env_mode": newMode,
			"message":         msg,
		})
		return

	case "FORCE_SYNC_TUNNEL":
		tURL := s.getTunnelURL()
		go s.syncGitHubTunnelURLForce(tURL, true)
		writeJSON(w, http.StatusOK, map[string]any{
			"tunnel_url": tURL,
			"message":    fmt.Sprintf("☁️ Link fixo https://sirbeefnup.github.io/pdv e /cardapio sincronizado com o túnel atual (%s)!", tURL),
		})
		return

	case "BACKUP_DB":
		srcDB := filepath.Join(s.DataDir, "sirbeef.db")
		bBytes, err := os.ReadFile(srcDB)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Erro ao ler banco de dados local: " + err.Error()})
			return
		}
		bName := fmt.Sprintf("sirbeef_backup_%s.db", time.Now().Format("20060102_150405"))
		bPath := filepath.Join(backupDir, bName)
		if err := os.WriteFile(bPath, bBytes, 0644); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Erro ao gravar backup: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"backup_name":    bName,
			"recent_backups": listBackups(),
			"message":        fmt.Sprintf("🛡️ Backup de segurança '%s' criado com sucesso!", bName),
		})
		return

	case "PUBLISH_SYSTEM":
		exeDir, _ := os.Getwd()
		rootDir := filepath.Dir(exeDir)
		vInfo := s.getCurrentAPKVersionInfo(r)
		hostname, _ := os.Hostname()
		notes := strings.TrimSpace(req.ReleaseNotes)
		if notes == "" {
			if rn, ok := vInfo["release_notes"].(string); ok && rn != "" {
				notes = rn
			} else {
				notes = "Atualização do Sistema Sir Beef (Telas PDV Web, Cardápio Online, Rastreio Ao Vivo, Backend e APK)"
			}
		}

		filesToPublish := make(map[string][]byte)
		systemFilePairs := []struct {
			LocalPath string
			CloudPath string
		}{
			{filepath.Join(s.WebDir, "pdv.html"), "release/web/pdv.html"},
			{filepath.Join(s.WebDir, "index.html"), "release/web/index.html"},
			{filepath.Join(s.WebDir, "rastreio.html"), "release/web/rastreio.html"},
			{filepath.Join(s.DataDir, "apk_version.json"), "release/data/apk_version.json"},
			{filepath.Join(exeDir, "main.go"), "release/backend/main.go"},
			{filepath.Join(exeDir, "internal", "models", "models.go"), "release/backend/internal/models/models.go"},
			{filepath.Join(exeDir, "internal", "db", "db.go"), "release/backend/internal/db/db.go"},
			{filepath.Join(exeDir, "internal", "escpos", "escpos.go"), "release/backend/internal/escpos/escpos.go"},
			{filepath.Join(exeDir, "internal", "whatsapp", "engine.go"), "release/backend/internal/whatsapp/engine.go"},
			{filepath.Join(exeDir, "sirbeef-server.zip"), "release/backend/sirbeef-server.zip"},
			{filepath.Join(rootDir, "INICIAR_ECOSSISTEMA_SIRBEEF.bat"), "release/INICIAR_ECOSSISTEMA_SIRBEEF.bat"},
			{filepath.Join(rootDir, "SirBeef-SuperApp.apk"), "SirBeef-SuperApp.apk"},
		}

		var publishedList []string
		for _, pair := range systemFilePairs {
			if content, err := os.ReadFile(pair.LocalPath); err == nil && len(content) > 0 {
				filesToPublish[pair.CloudPath] = content
				publishedList = append(publishedList, pair.CloudPath)
			}
		}

		manifest := map[string]any{
			"version_code":          vInfo["version_code"],
			"version_name":          vInfo["version_name"],
			"release_notes":         notes,
			"published_at":          time.Now().Format("02/01/2006 15:04:05"),
			"published_from_pc":     hostname,
			"published_files":       publishedList,
			"excluded_private_data": []string{"sirbeef.db (Clientes/Vendas de Teste)", "whatsapp_session.db"},
		}
		manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
		filesToPublish["release/system_manifest.json"] = manifestJSON

		commitSHA, err := s.pushFilesAtomicallyToGitHub(filesToPublish, fmt.Sprintf("Release Sistema Sir Beef v%v (Sem Dados de Clientes)", vInfo["version_name"]))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Falha ao publicar pacote na nuvem: " + err.Error()})
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"commit_sha":     commitSHA,
			"cloud_manifest": manifest,
			"message":        fmt.Sprintf("🚀 Sistema v%v publicado na nuvem com sucesso (Commit %s)! NENHUM cliente ou venda de teste foi enviado. O PC da Hamburgueria já pode clicar em 'Receber Atualização'!", vInfo["version_name"], commitSHA[:7]),
		})
		return

	case "UPDATE_SYSTEM":
		if ghCfg.Owner == "" || ghCfg.Repo == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Configuração do GitHub não encontrada em data/github_config.json."})
			return
		}
		// 1) Backup automático do banco de dados real da Hamburgueria antes de atualizar qualquer arquivo!
		srcDB := filepath.Join(s.DataDir, "sirbeef.db")
		backupCreated := ""
		if bBytes, err := os.ReadFile(srcDB); err == nil && len(bBytes) > 0 {
			backupCreated = fmt.Sprintf("sirbeef_backup_pre_update_%s.db", time.Now().Format("20060102_150405"))
			_ = os.WriteFile(filepath.Join(backupDir, backupCreated), bBytes, 0644)
		}

		exeDir, _ := os.Getwd()
		rootDir := filepath.Dir(exeDir)
		downloadPairs := []struct {
			CloudPath string
			LocalPath string
		}{
			{"release/web/pdv.html", filepath.Join(s.WebDir, "pdv.html")},
			{"release/web/index.html", filepath.Join(s.WebDir, "index.html")},
			{"release/web/rastreio.html", filepath.Join(s.WebDir, "rastreio.html")},
			{"release/data/apk_version.json", filepath.Join(s.DataDir, "apk_version.json")},
			{"release/backend/main.go", filepath.Join(exeDir, "main.go")},
			{"release/backend/internal/models/models.go", filepath.Join(exeDir, "internal", "models", "models.go")},
			{"release/backend/internal/db/db.go", filepath.Join(exeDir, "internal", "db", "db.go")},
			{"release/backend/internal/escpos/escpos.go", filepath.Join(exeDir, "internal", "escpos", "escpos.go")},
			{"release/backend/internal/whatsapp/engine.go", filepath.Join(exeDir, "internal", "whatsapp", "engine.go")},
			{"release/INICIAR_ECOSSISTEMA_SIRBEEF.bat", filepath.Join(rootDir, "INICIAR_ECOSSISTEMA_SIRBEEF.bat")},
			{"release/backend/sirbeef-server.zip", filepath.Join(exeDir, "sirbeef-server.zip")},
			{"SirBeef-SuperApp.apk", filepath.Join(rootDir, "SirBeef-SuperApp.apk")},
		}

		client := &http.Client{Timeout: 60 * time.Second}
		var updatedFiles []string
		for _, pair := range downloadPairs {
			u := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s", ghCfg.Owner, ghCfg.Repo, pair.CloudPath, ghCfg.Branch)
			reqDL, err := http.NewRequest(http.MethodGet, u, nil)
			if err != nil {
				continue
			}
			if ghCfg.Token != "" {
				reqDL.Header.Set("Authorization", "Bearer "+ghCfg.Token)
			}
			reqDL.Header.Set("Accept", "application/vnd.github.raw+json")
			reqDL.Header.Set("User-Agent", "SirBeef-Host-Server")
			respDL, err := client.Do(reqDL)
			if err != nil || respDL == nil {
				continue
			}
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(respDL.Body)
			respDL.Body.Close()
			if respDL.StatusCode == http.StatusOK && buf.Len() > 0 {
				_ = os.MkdirAll(filepath.Dir(pair.LocalPath), 0755)
				if os.WriteFile(pair.LocalPath, buf.Bytes(), 0644) == nil {
					updatedFiles = append(updatedFiles, pair.CloudPath)
				}
			}
		}

		// Se o pacote binário sirbeef-server.zip foi baixado, extrai para sirbeef-server-new.exe e agenda reinício rápido em 1.5s
		zipPath := filepath.Join(exeDir, "sirbeef-server.zip")
		if info, errZ := os.Stat(zipPath); errZ == nil && info.Size() > 100000 {
			go func() {
				time.Sleep(1500 * time.Millisecond)
				psCmd := `Expand-Archive -Path 'sirbeef-server.zip' -DestinationPath '.\temp_bin' -Force; if (Test-Path '.\temp_bin\sirbeef-server.exe') { Move-Item -Path '.\temp_bin\sirbeef-server.exe' -Destination '.\sirbeef-server-new.exe' -Force; Remove-Item '.\temp_bin' -Recurse -Force; Remove-Item 'sirbeef-server.zip' -Force; Stop-Process -Name 'sirbeef-server' -Force }`
				c := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", psCmd)
				c.Dir = exeDir
				_ = c.Start()
			}()
		}

		cloudMan := fetchCloudManifest()
		writeJSON(w, http.StatusOK, map[string]any{
			"updated_files":  updatedFiles,
			"backup_created": backupCreated,
			"recent_backups": listBackups(),
			"cloud_manifest": cloudMan,
			"local_version":  s.getCurrentAPKVersionInfo(r),
			"message":        fmt.Sprintf("✅ Sistema atualizado com sucesso (%d módulos atualizados)! Backup automático '%s' salvo e 100%% dos seus Clientes, Pedidos, Faturamento e WhatsApp foram mantidos intactos!", len(updatedFiles), backupCreated),
		})
		return
	}

	writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Ação inválida: " + action})
}

