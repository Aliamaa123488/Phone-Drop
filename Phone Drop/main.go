package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFiles embed.FS

const maxUploadBytes int64 = 10 << 30 // 10 GiB per request

type app struct {
	folder          string
	settingsPath    string
	code            string
	qr              [][]bool
	qrURL           string
	lockMu          sync.Mutex
	badCodes        map[string]attempt
	settingsMu      sync.Mutex
	darkMode        bool
	server          *http.Server
	clientMu        sync.Mutex
	clients         map[string]time.Time
	seenClient      bool
	idleTimer       *time.Timer
	activeTransfers int
	transferMu      sync.Mutex
	paused          bool
	resumeCh        chan struct{}
}

type attempt struct {
	count int
	until time.Time
}

type fileInfo struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modified"`
}

func main() {
	port := flag.Int("port", 8765, "local sharing port")
	folderFlag := flag.String("folder", "", "folder to use for shared files")
	flag.Parse()
	if *port < 1 || *port > 65535 {
		fatalAppError("Port must be between 1 and 65535.")
	}

	folder := *folderFlag
	if folder == "" {
		docs, err := os.UserHomeDir()
		if err != nil {
			fatalAppError(fmt.Sprintf("Could not find your user folder: %v", err))
		}
		folder = filepath.Join(docs, "Downloads", "Phone Drop")
	}
	folder, err := filepath.Abs(folder)
	if err != nil {
		fatalAppError(fmt.Sprintf("Could not resolve the Phone Drop folder: %v", err))
	}
	if err := os.MkdirAll(folder, 0755); err != nil {
		fatalAppError(fmt.Sprintf("Could not create the Phone Drop folder: %v", err))
	}

	code, err := newPairCode()
	if err != nil {
		fatalAppError(fmt.Sprintf("Could not create a pairing code: %v", err))
	}

	lanIP, _ := preferredLANIP()
	if lanIP == nil {
		lanIP = net.ParseIP("127.0.0.1")
	}
	listener, actualPort, err := listenOnAvailablePort(lanIP, *port)
	if err != nil {
		fatalAppError(err.Error())
	}

	lanURL := fmt.Sprintf("http://%s:%d/?code=%s", lanIP.String(), actualPort, code)
	qr, err := encodeQR(lanURL)
	if err != nil {
		fatalAppError(fmt.Sprintf("Could not create a QR code: %v", err))
	}
	settingsPath := filepath.Join(folder, ".phonedrop-settings.json")
	a := &app{folder: folder, settingsPath: settingsPath, code: code, qr: qr, qrURL: lanURL, badCodes: make(map[string]attempt), darkMode: readDarkMode(settingsPath), clients: make(map[string]time.Time)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.home)
	mux.HandleFunc("/qr.svg", a.qrSVG)
	mux.HandleFunc("/assets/", a.asset)
	mux.HandleFunc("/api/files", a.withPairCode(a.listFiles))
	mux.HandleFunc("/api/upload", a.withPairCode(a.uploadFiles))
	mux.HandleFunc("/api/download", a.withPairCode(a.downloadFile))
	mux.HandleFunc("/api/settings", a.withPairCode(a.settings))
	mux.HandleFunc("/api/clear", a.withPairCode(a.clearFiles))
	mux.HandleFunc("/api/pause", a.withPairCode(a.pauseTransfers))
	mux.HandleFunc("/api/resume", a.withPairCode(a.resumeTransfers))
	mux.HandleFunc("/api/disconnect", a.withPairCode(a.disconnectClient))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	a.server = server
	go a.monitorClients()
	openBrowser(lanURL)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatalAppError(fmt.Sprintf("PhoneDrop stopped unexpectedly: %v", err))
	}
}

func newPairCode() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return fmt.Sprintf("%08d", n%100000000), nil
}

func listenOnAvailablePort(ip net.IP, requested int) (net.Listener, int, error) {
	const attempts = 16
	var lastErr error
	lastPort := requested
	for offset := 0; offset < attempts && requested+offset <= 65535; offset++ {
		port := requested + offset
		listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err == nil {
			return listener, port, nil
		}
		lastErr = err
		lastPort = port
	}
	return nil, 0, fmt.Errorf("could not listen on ports %d-%d: %w", requested, lastPort, lastErr)
}

func preferredLANIP() (net.IP, string) {
	type candidate struct {
		ip       net.IP
		iface    string
		priority int
	}
	var found []candidate
	ifs, _ := net.Interfaces()
	for _, item := range ifs {
		if item.Flags&net.FlagUp == 0 || item.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := item.Addrs()
		for _, addr := range addrs {
			var ip net.IP
			switch a := addr.(type) {
			case *net.IPNet:
				ip = a.IP
			case *net.IPAddr:
				ip = a.IP
			}
			ip = ip.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			priority := 4
			if ip.IsPrivate() {
				priority = 2
			}
			name := strings.ToLower(item.Name)
			if strings.Contains(name, "wi-fi") || strings.Contains(name, "wifi") || strings.Contains(name, "wireless") {
				priority--
			} else if strings.Contains(name, "ethernet") {
				priority--
			}
			if strings.Contains(name, "virtual") || strings.Contains(name, "vpn") || strings.Contains(name, "loopback") {
				priority += 3
			}
			found = append(found, candidate{ip, item.Name, priority})
		}
	}
	if len(found) == 0 {
		return nil, ""
	}
	sort.Slice(found, func(i, j int) bool { return found[i].priority < found[j].priority })
	return found[0].ip, found[0].iface
}

func openBrowser(url string) {
	if err := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start(); err != nil {
		showAppMessage("PhoneDrop is running", fmt.Sprintf("Open this address in a browser:\n%s", url))
	}
}

func fatalAppError(message string) {
	showAppMessage("PhoneDrop", message)
	os.Exit(1)
}

func (a *app) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !a.checkPairCode(r) {
		http.Error(w, "Open the paired PhoneDrop address or scan its QR code", http.StatusUnauthorized)
		return
	}
	data, err := webFiles.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "UI could not be loaded", http.StatusInternalServerError)
		return
	}
	secureHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page := strings.ReplaceAll(string(data), "__PAIR_CODE__", html.EscapeString(a.code))
	page = strings.ReplaceAll(page, "__PHONE_URL__", html.EscapeString(a.qrURL))
	page = strings.ReplaceAll(page, "__THEME__", a.theme())
	w.Write([]byte(page))
}

func readDarkMode(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var settings struct {
		Theme string `json:"theme"`
	}
	if json.Unmarshal(data, &settings) != nil {
		return false
	}
	return settings.Theme == "dark"
}

func (a *app) theme() string {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	if a.darkMode {
		return "dark"
	}
	return "light"
}

func (a *app) settings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]string{"theme": a.theme()})
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		var settings struct {
			Theme string `json:"theme"`
		}
		if err := json.NewDecoder(r.Body).Decode(&settings); err != nil || (settings.Theme != "dark" && settings.Theme != "light") {
			http.Error(w, "theme must be dark or light", http.StatusBadRequest)
			return
		}
		data, _ := json.Marshal(settings)
		a.settingsMu.Lock()
		err := os.WriteFile(a.settingsPath, data, 0644)
		if err == nil {
			a.darkMode = settings.Theme == "dark"
		}
		a.settingsMu.Unlock()
		if err != nil {
			http.Error(w, "could not save the theme setting", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(settings)
	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *app) qrSVG(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !a.checkPairCode(r) {
		http.Error(w, "pairing code required", http.StatusUnauthorized)
		return
	}
	secureHeaders(w)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	const quiet = 4
	size := len(a.qr)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges"><rect width="100%%" height="100%%" fill="#fff"/><g fill="#13243b">`, size+quiet*2, size+quiet*2)
	for y, row := range a.qr {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1"/>`, x+quiet, y+quiet)
			}
		}
	}
	b.WriteString("</g></svg>")
	w.Write([]byte(b.String()))
}

func (a *app) asset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name != "cloud-upload.svg" && name != "cloud-download.svg" {
		http.NotFound(w, r)
		return
	}
	data, err := webFiles.ReadFile("web/assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	secureHeaders(w)
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Write(data)
}

func (a *app) checkPairCode(r *http.Request) bool {
	provided := r.Header.Get("X-Pair-Code")
	if provided == "" {
		provided = r.URL.Query().Get("code")
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	a.lockMu.Lock()
	defer a.lockMu.Unlock()
	state := a.badCodes[ip]
	blocked := time.Now().Before(state.until)
	if subtle.ConstantTimeCompare([]byte(provided), []byte(a.code)) != 1 || blocked {
		if !blocked {
			state.count++
			if state.count >= 8 {
				state.until = time.Now().Add(30 * time.Second)
				state.count = 0
			}
			a.badCodes[ip] = state
		}
		return false
	}
	delete(a.badCodes, ip)
	return true
}

func secureHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}

func (a *app) withPairCode(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secureHeaders(w)
		if !a.checkPairCode(r) {
			http.Error(w, "Pairing code required or incorrect", http.StatusUnauthorized)
			return
		}
		a.registerClient(r.Header.Get("X-Client-ID"))
		next(w, r)
	}
}

func (a *app) registerClient(id string) {
	if id == "" || len(id) > 128 {
		return
	}
	a.clientMu.Lock()
	a.clients[id] = time.Now()
	a.seenClient = true
	if a.idleTimer != nil {
		a.idleTimer.Stop()
		a.idleTimer = nil
	}
	a.clientMu.Unlock()
}

func (a *app) disconnectClient(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("client")
	if id == "" || len(id) > 128 {
		http.Error(w, "client id required", http.StatusBadRequest)
		return
	}
	a.clientMu.Lock()
	delete(a.clients, id)
	a.seenClient = true
	a.scheduleIfIdleLocked()
	a.clientMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) scheduleIdleShutdownLocked(delay time.Duration) {
	if a.idleTimer != nil {
		return
	}
	a.idleTimer = time.AfterFunc(delay, a.shutdownIfIdle)
}

func (a *app) scheduleIfIdleLocked() {
	if a.seenClient && len(a.clients) == 0 && a.activeTransfers == 0 {
		a.scheduleIdleShutdownLocked(3 * time.Second)
	}
}

func (a *app) beginTransfer() func() {
	a.clientMu.Lock()
	a.activeTransfers++
	if a.idleTimer != nil {
		a.idleTimer.Stop()
		a.idleTimer = nil
	}
	a.clientMu.Unlock()
	return func() {
		a.clientMu.Lock()
		a.activeTransfers--
		a.scheduleIfIdleLocked()
		a.clientMu.Unlock()
	}
}

func (a *app) monitorClients() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		a.clientMu.Lock()
		for id, lastSeen := range a.clients {
			if now.Sub(lastSeen) > 5*time.Minute {
				delete(a.clients, id)
			}
		}
		a.scheduleIfIdleLocked()
		a.clientMu.Unlock()
	}
}

func (a *app) shutdownIfIdle() {
	a.clientMu.Lock()
	a.idleTimer = nil
	now := time.Now()
	for id, lastSeen := range a.clients {
		if now.Sub(lastSeen) > 5*time.Minute {
			delete(a.clients, id)
		}
	}
	if !a.seenClient || len(a.clients) != 0 || a.activeTransfers != 0 {
		a.clientMu.Unlock()
		return
	}
	a.clientMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.server.Shutdown(ctx); err != nil {
		a.server.Close()
	}
}

func (a *app) setPaused(paused bool) {
	a.transferMu.Lock()
	defer a.transferMu.Unlock()
	if paused == a.paused {
		return
	}
	if paused {
		a.paused = true
		a.resumeCh = make(chan struct{})
		return
	}
	a.paused = false
	close(a.resumeCh)
	a.resumeCh = nil
}

func (a *app) isPaused() bool {
	a.transferMu.Lock()
	defer a.transferMu.Unlock()
	return a.paused
}

func (a *app) waitForTransfers(ctx context.Context) error {
	for {
		a.transferMu.Lock()
		if !a.paused {
			a.transferMu.Unlock()
			return nil
		}
		resume := a.resumeCh
		a.transferMu.Unlock()
		select {
		case <-resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (a *app) pauseTransfers(w http.ResponseWriter, r *http.Request) {
	a.setTransferState(w, r, true)
}

func (a *app) resumeTransfers(w http.ResponseWriter, r *http.Request) {
	a.setTransferState(w, r, false)
}

func (a *app) setTransferState(w http.ResponseWriter, r *http.Request, paused bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.setPaused(paused)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]bool{"paused": paused})
}

func (a *app) listFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.isPaused() {
		w.Header().Set("X-PhoneDrop-Paused", "true")
	} else {
		w.Header().Set("X-PhoneDrop-Paused", "false")
	}
	entries, err := os.ReadDir(a.folder)
	if err != nil {
		http.Error(w, "could not list share folder", http.StatusInternalServerError)
		return
	}
	items := make([]fileInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.EqualFold(entry.Name(), filepath.Base(a.settingsPath)) {
			continue
		}
		info, err := entry.Info()
		if err == nil && info.Mode().IsRegular() {
			items = append(items, fileInfo{Name: entry.Name(), Size: info.Size(), ModTime: info.ModTime()})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ModTime.After(items[j].ModTime) })
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(items)
}

func (a *app) uploadFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := a.waitForTransfers(r.Context()); err != nil {
		return
	}
	endTransfer := a.beginTransfer()
	defer endTransfer()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	reader, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "expected a multipart file upload", http.StatusBadRequest)
		return
	}
	var saved []string
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			status := http.StatusBadRequest
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "upload could not be read", status)
			return
		}
		if part.FileName() == "" || part.FormName() != "files" {
			part.Close()
			continue
		}
		name := cleanFileName(part.FileName())
		if name == "" {
			part.Close()
			continue
		}
		file, target, err := createUniqueFile(a.folder, name)
		if err != nil {
			part.Close()
			http.Error(w, "could not save file", http.StatusInternalServerError)
			return
		}
		_, copyErr := io.Copy(pausableWriter{app: a, ctx: r.Context(), dst: file}, part)
		closeErr := file.Close()
		part.Close()
		if copyErr != nil || closeErr != nil {
			os.Remove(target)
			status := http.StatusInternalServerError
			var maxErr *http.MaxBytesError
			if errors.As(copyErr, &maxErr) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "file could not be saved; check free disk space and upload size", status)
			return
		}
		saved = append(saved, filepath.Base(target))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"uploaded": saved})
}

func (a *app) downloadFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := a.waitForTransfers(r.Context()); err != nil {
		return
	}
	endTransfer := a.beginTransfer()
	defer endTransfer()
	name := cleanFileName(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	f, err := os.Open(filepath.Join(a.folder, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	secureHeaders(w)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(pausableResponseWriter{ResponseWriter: w, app: a, ctx: r.Context()}, r, name, info.ModTime(), f)
}

type pausableWriter struct {
	app *app
	ctx context.Context
	dst io.Writer
}

func (w pausableWriter) Write(p []byte) (int, error) {
	if err := w.app.waitForTransfers(w.ctx); err != nil {
		return 0, err
	}
	return w.dst.Write(p)
}

type pausableResponseWriter struct {
	http.ResponseWriter
	app *app
	ctx context.Context
}

func (w pausableResponseWriter) Write(p []byte) (int, error) {
	if err := w.app.waitForTransfers(w.ctx); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(p)
}

func (a *app) clearFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := a.waitForTransfers(r.Context()); err != nil {
		return
	}
	entries, err := os.ReadDir(a.folder)
	if err != nil {
		http.Error(w, "could not read the Phone Drop folder", http.StatusInternalServerError)
		return
	}
	deleted := 0
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), filepath.Base(a.settingsPath)) || entry.IsDir() {
			continue
		}
		path := filepath.Join(a.folder, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil {
			http.Error(w, "some files could not be removed", http.StatusInternalServerError)
			return
		}
		deleted++
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"deleted": deleted})
}

func cleanFileName(raw string) string {
	name := path.Base(strings.ReplaceAll(raw, "\\", "/"))
	name = strings.TrimSpace(name)
	name = strings.TrimRight(name, ". ")
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	if name == "." || name == ".." || name == "" {
		return ""
	}
	base := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || reservedWindowsName(base) {
		name = "_" + name
	}
	return name
}

func reservedWindowsName(base string) bool {
	for i := 1; i <= 9; i++ {
		if base == fmt.Sprintf("COM%d", i) || base == fmt.Sprintf("LPT%d", i) {
			return true
		}
	}
	return false
}

func createUniqueFile(folder, name string) (*os.File, string, error) {
	for i := 0; i < 10000; i++ {
		candidate := name
		if i > 0 {
			ext := filepath.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			candidate = fmt.Sprintf("%s (%d)%s", stem, i+1, ext)
		}
		target := filepath.Join(folder, candidate)
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			return f, target, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("too many files with this name")
}
