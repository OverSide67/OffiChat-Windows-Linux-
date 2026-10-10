// Copyright 2026 OverSide67
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	webAddr      = "127.0.0.1:8080"
	tcpPort      = "5000"
	maxFileBytes = 15 << 20
	btUUID       = "8f3c1a2e-5b7d-4e9a-9c1f-2d6e4b8a7c10"
	btDevPath    = "/dev/rfcomm0"
)

var recvDir = "received"

// ======================= Bluetooth (клиент, только Linux) =======================

// btLink оборачивает файл /dev/rfcomm0 и умеет его освободить
type btLink struct {
	f *os.File
}

func (b *btLink) Read(p []byte) (int, error)  { return b.f.Read(p) }
func (b *btLink) Write(p []byte) (int, error) { return b.f.Write(p) }
func (b *btLink) Close() error {
	b.f.Close()
	rfcommRelease()
	return nil
}

// rfcommCmd запускает утилиту rfcomm, при необходимости через sudo
func rfcommCmd(args ...string) error {
	var cmd *exec.Cmd
	if os.Geteuid() == 0 {
		cmd = exec.Command("rfcomm", args...)
	} else {
		cmd = exec.Command("sudo", append([]string{"rfcomm"}, args...)...)
	}
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func rfcommRelease() {
	if runtime.GOOS == "linux" {
		_ = rfcommCmd("release", "0")
	}
}

// btConnect привязывает /dev/rfcomm0 к телефону и открывает его
func btConnect(mac string, channel uint8) (io.ReadWriteCloser, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("Bluetooth работает только на Linux. Используйте Wi-Fi")
	}
	if _, err := exec.LookPath("rfcomm"); err != nil {
		return nil, fmt.Errorf("не найдена утилита rfcomm: sudo apt install bluez")
	}
	rfcommRelease()
	if err := rfcommCmd("bind", "0", mac, strconv.Itoa(int(channel))); err != nil {
		return nil, fmt.Errorf("rfcomm bind не удался (нужен sudo и спаренное устройство): %v", err)
	}
	// открытие блокируется до установки соединения, поэтому ждём с таймаутом
	type result struct {
		f   *os.File
		err error
	}
	ch := make(chan result, 1)
	go func() {
		f, err := os.OpenFile(btDevPath, os.O_RDWR, 0)
		ch <- result{f, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			rfcommRelease()
			return nil, r.err
		}
		return &btLink{f: r.f}, nil
	case <-time.After(20 * time.Second):
		rfcommRelease()
		return nil, fmt.Errorf("телефон не ответил за 20 секунд. Проверьте, что на нём запущен Bluetooth-хост")
	}
}

// findChannel ищет канал сервиса OffiChat через sdptool
func findChannel(mac string) (uint8, error) {
	if runtime.GOOS != "linux" {
		return 0, fmt.Errorf("автопоиск канала только на Linux")
	}
	out, err := exec.Command("sdptool", "records", mac).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("sdptool: %v", err)
	}
	re := regexp.MustCompile(`Channel:\s*(\d+)`)
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(strings.ToLower(line), btUUID) {
			found = true
			continue
		}
		if found {
			if m := re.FindStringSubmatch(line); m != nil {
				n, _ := strconv.Atoi(m[1])
				return uint8(n), nil
			}
		}
	}
	return 0, fmt.Errorf("сервис OffiChat не найден в SDP")
}

// ======================= состояние =======================

type message struct {
	ID   int    `json:"id"`
	From string `json:"from"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	URL  string `json:"url"`
	Time string `json:"time"`
}

type connReq struct {
	Transport string `json:"transport"`
	Role      string `json:"role"`
	IP        string `json:"ip"`
	MAC       string `json:"mac"`
	Channel   int    `json:"channel"`
}

type stateResp struct {
	State    string    `json:"state"`
	Status   string    `json:"status"`
	Peer     string    `json:"peer"`
	LocalIP  string    `json:"localIp"`
	Messages []message `json:"messages"`
}

type app struct {
	mu       sync.Mutex
	gen      int
	conn     io.ReadWriteCloser
	listener net.Listener
	state    string
	status   string
	peer     string
	messages []message
}

var st = &app{state: "idle", status: "Не подключено"}

func (a *app) addLocked(from, kind, text, url string) {
	a.messages = append(a.messages, message{
		ID: len(a.messages), From: from, Kind: kind, Text: text, URL: url,
		Time: time.Now().Format("15:04"),
	})
}

func (a *app) add(from, kind, text, url string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addLocked(from, kind, text, url)
}

func (a *app) setState(gen int, state, status string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen {
		return
	}
	a.state, a.status = state, status
}

func (a *app) fail(gen int, msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen {
		return
	}
	a.state = "idle"
	a.status = "Ошибка: " + msg
	a.peer = ""
	a.addLocked("system", "system", "Ошибка: "+msg, "")
}

func (a *app) setListener(gen int, ln net.Listener) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen != a.gen {
		return false
	}
	a.listener = ln
	return true
}

func (a *app) disconnect() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gen++
	if a.listener != nil {
		a.listener.Close()
		a.listener = nil
	}
	if a.conn != nil {
		a.conn.Close()
		a.conn = nil
	}
	a.state = "idle"
	a.status = "Не подключено"
	a.peer = ""
	return a.gen
}

func (a *app) attach(conn io.ReadWriteCloser, peer string, gen int) {
	a.mu.Lock()
	if gen != a.gen {
		a.mu.Unlock()
		conn.Close()
		return
	}
	a.conn = conn
	a.state = "connected"
	a.status = "Подключено"
	a.peer = peer
	a.addLocked("system", "system", "Подключено: "+peer, "")
	a.mu.Unlock()
	go a.readLoop(conn, gen)
}

func (a *app) readLoop(conn io.ReadWriteCloser, gen int) {
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), maxFileBytes*2+1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "F|") {
			a.receiveFile(line)
		} else {
			a.add("peer", "text", line, "")
		}
	}
	reason := "Соединение разорвано"
	if err := sc.Err(); err != nil {
		reason = "Обрыв связи: " + err.Error()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen == a.gen && a.conn == conn {
		a.conn = nil
		a.state = "idle"
		a.status = reason
		a.peer = ""
		a.addLocked("system", "system", reason, "")
	}
}

func (a *app) send(text string) error {
	a.mu.Lock()
	c := a.conn
	a.mu.Unlock()
	if c == nil {
		return fmt.Errorf("нет подключения")
	}
	if _, err := io.WriteString(c, text+"\n"); err != nil {
		return err
	}
	a.add("me", "text", text, "")
	return nil
}

func (a *app) sendFile(name string, data []byte) error {
	a.mu.Lock()
	c := a.conn
	a.mu.Unlock()
	if c == nil {
		return fmt.Errorf("нет подключения")
	}
	safe := safeName(name)
	saved, err := saveFile("sent_"+safe, data)
	if err != nil {
		return err
	}
	line := "F|" + safe + "|" + base64.StdEncoding.EncodeToString(data) + "\n"
	if _, err := io.WriteString(c, line); err != nil {
		return err
	}
	a.add("me", kindOf(safe), safe, "/files/"+saved)
	return nil
}

func (a *app) receiveFile(line string) {
	parts := strings.SplitN(line, "|", 3)
	if len(parts) < 3 {
		return
	}
	data, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		a.add("system", "system", "Файл повреждён при приёме", "")
		return
	}
	name := safeName(parts[1])
	saved, err := saveFile(name, data)
	if err != nil {
		a.add("system", "system", "Не удалось сохранить файл: "+err.Error(), "")
		return
	}
	a.add("peer", kindOf(name), name, "/files/"+saved)
}

func (a *app) connect(req connReq) {
	gen := a.disconnect()
	go a.run(req, gen)
}

func (a *app) run(req connReq, gen int) {
	var conn io.ReadWriteCloser
	var err error
	peer := ""

	switch {
	case req.Transport == "bt" && req.Role == "host":
		a.fail(gen, "Bluetooth-хост на ПК не поддерживается. Сделайте хостом телефон, а ПК подключите как клиент")
		return

	case req.Transport == "bt":
		mac := strings.ToUpper(strings.TrimSpace(req.MAC))
		if mac == "" {
			a.fail(gen, "Введите MAC телефона")
			return
		}
		ch := uint8(req.Channel)
		if ch == 0 {
			a.setState(gen, "connecting", "Ищем сервис OffiChat на "+mac+"...")
			found, ferr := findChannel(mac)
			if ferr != nil {
				a.fail(gen, ferr.Error()+". Укажите канал вручную")
				return
			}
			ch = found
		}
		a.setState(gen, "connecting", fmt.Sprintf("Подключаемся к %s, канал %d...", mac, ch))
		conn, err = btConnect(mac, ch)
		peer = mac

	case req.Role == "host":
		ln, lerr := net.Listen("tcp", "0.0.0.0:"+tcpPort)
		if lerr != nil {
			a.fail(gen, lerr.Error())
			return
		}
		if !a.setListener(gen, ln) {
			ln.Close()
			return
		}
		a.setState(gen, "waiting", "Wi-Fi: ждём подключения на "+localIP()+":"+tcpPort)
		c, aerr := ln.Accept()
		ln.Close()
		if aerr != nil {
			a.fail(gen, aerr.Error())
			return
		}
		conn = c
		peer = c.RemoteAddr().String()

	default:
		ip := strings.TrimSpace(req.IP)
		if ip == "" {
			ip = "192.168.43.1"
		}
		host, _, serr := net.SplitHostPort(ip)
		if serr != nil {
			host = ip
		}
		target := net.JoinHostPort(host, tcpPort)
		a.setState(gen, "connecting", "Подключаемся к "+target+"...")
		conn, err = net.DialTimeout("tcp", target, 10*time.Second)
		peer = host
	}

	if err != nil {
		a.fail(gen, err.Error())
		return
	}
	a.attach(conn, peer, gen)
}

// ======================= файлы =======================

func safeName(n string) string {
	n = filepath.Base(strings.TrimSpace(n))
	n = strings.Map(func(r rune) rune {
		if r == '|' || r == '\n' || r == '\r' || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, n)
	if n == "" || n == "." {
		n = "file"
	}
	return n
}

func kindOf(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return "image"
	case ".mp4", ".webm", ".mov", ".mkv", ".3gp":
		return "video"
	}
	return "file"
}

func saveFile(name string, data []byte) (string, error) {
	if err := os.MkdirAll(recvDir, 0o755); err != nil {
		return "", err
	}
	fname := time.Now().Format("20060102_150405") + "_" + name
	if err := os.WriteFile(filepath.Join(recvDir, fname), data, 0o644); err != nil {
		return "", err
	}
	return fname, nil
}

// ======================= сеть и сигнал =======================

// wifiSignal читает качество Wi-Fi из /proc (только Linux), иначе -1
func wifiSignal() int {
	if runtime.GOOS != "linux" {
		return -1
	}
	data, err := os.ReadFile("/proc/net/wireless")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || !strings.HasSuffix(f[0], ":") {
			continue
		}
		link, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "."), 64)
		if err != nil {
			continue
		}
		p := int(link * 100 / 70)
		if p > 100 {
			p = 100
		}
		return p
	}
	return -1
}

func localIP() string {
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

// ======================= HTTP =======================

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func handleConnect(w http.ResponseWriter, r *http.Request) {
	var req connReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "неверный запрос", http.StatusBadRequest)
		return
	}
	st.connect(req)
	writeJSON(w, map[string]bool{"ok": true})
}

func handleDisconnect(w http.ResponseWriter, r *http.Request) {
	st.disconnect()
	writeJSON(w, map[string]bool{"ok": true})
}

func handleSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "неверный запрос", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(strings.ReplaceAll(body.Text, "\n", " "))
	if text == "" {
		http.Error(w, "пустое сообщение", http.StatusBadRequest)
		return
	}
	if err := st.send(text); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func handleSendFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxFileBytes*4/3)+4096)
	var body struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "файл слишком большой (лимит 15 МБ)", http.StatusRequestEntityTooLarge)
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil || len(data) == 0 {
		http.Error(w, "неверные данные файла", http.StatusBadRequest)
		return
	}
	if len(data) > maxFileBytes {
		http.Error(w, "файл больше 15 МБ", http.StatusRequestEntityTooLarge)
		return
	}
	if err := st.sendFile(body.Name, data); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func handleState(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	st.mu.Lock()
	defer st.mu.Unlock()
	msgs := []message{}
	for _, m := range st.messages {
		if m.ID >= since {
			msgs = append(msgs, m)
		}
	}
	writeJSON(w, stateResp{
		State: st.state, Status: st.status, Peer: st.peer,
		LocalIP: localIP(), Messages: msgs,
	})
}

func handleSignal(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]int{"level": wifiSignal()})
}

func handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, pageHTML)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func main() {
	if home, err := os.UserHomeDir(); err == nil {
		recvDir = filepath.Join(home, "OffiChat", "received")
	}
	os.MkdirAll(recvDir, 0o755)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handlePage)
	mux.HandleFunc("/api/connect", handleConnect)
	mux.HandleFunc("/api/disconnect", handleDisconnect)
	mux.HandleFunc("/api/send", handleSend)
	mux.HandleFunc("/api/sendfile", handleSendFile)
	mux.HandleFunc("/api/state", handleState)
	mux.HandleFunc("/api/signal", handleSignal)
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(recvDir))))

	url := "http://localhost:8080"
	fmt.Println("OffiChat запущен:", url)
	fmt.Println("Полученные файлы:", recvDir)
	fmt.Println("Остановка: Ctrl+C")
	go func() {
		time.Sleep(300 * time.Millisecond)
		openBrowser(url)
	}()

	srv := &http.Server{Addr: webAddr, Handler: mux}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Println("Ошибка сервера:", err)
	}
}

const pageHTML = `<!DOCTYPE html>
<html lang="ru">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>OffiChat</title>
<style>
* { box-sizing: border-box; }
html, body { height: 100%; margin: 0; }
body { background: #0F172A; color: #F1F5F9; font-family: -apple-system, "Segoe UI", Roboto, sans-serif; }
.app { height: 100%; display: flex; flex-direction: column; position: relative; }
header { background: #0B1120; display: flex; align-items: center; gap: 12px; padding: 10px 16px; border-bottom: 1px solid #1E293B; }
.dot { width: 10px; height: 10px; border-radius: 50%; background: #475569; flex-shrink: 0; }
.dot.waiting, .dot.connecting { background: #FACC15; animation: p 1.2s infinite; }
.dot.connected { background: #22C55E; }
@keyframes p { 50% { opacity: .3; } }
.hinfo { min-width: 180px; max-width: 320px; }
#tvPeer { font-weight: 700; font-size: 15px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
#tvStatus { font-size: 12px; color: #94A3B8; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.spacer { flex: 1; }
.sig { display: flex; align-items: center; gap: 6px; font-size: 12px; color: #94A3B8; white-space: nowrap; }
.bars { display: flex; align-items: flex-end; gap: 2px; height: 14px; }
.bars i { width: 4px; background: #334155; border-radius: 1px; }
.bars i.on { background: #22C55E; }
.localip { font-size: 12px; color: #64748B; white-space: nowrap; }
.gear { background: none; border: 0; color: #A5B4FC; font-size: 20px; cursor: pointer; padding: 6px 10px; border-radius: 10px; }
.gear:hover { background: #1E293B; }
#settings { background: #1E293B; padding: 12px 16px; display: flex; flex-direction: column; gap: 12px; border-bottom: 1px solid #0B1120; }
#settings.hidden { display: none; }
.bar { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.grp { display: flex; align-items: center; gap: 8px; }
.glbl { font-size: 11px; color: #94A3B8; text-transform: uppercase; letter-spacing: .06em; white-space: nowrap; }
input { background: #0F172A; border: 1px solid transparent; color: #F1F5F9; border-radius: 10px; padding: 9px 12px; font-size: 14px; outline: none; min-width: 0; }
input:focus { border-color: #6366F1; }
#myName { width: 150px; }
#ip, #mac { width: 210px; }
#channel { width: 170px; }
#cName { width: 160px; }
.tog { background: #0F172A; color: #94A3B8; border: 0; border-radius: 10px; padding: 9px 14px; font-size: 14px; cursor: pointer; white-space: nowrap; }
.tog.on { background: #6366F1; color: #fff; }
.field { display: none; }
.field.show { display: block; }
.btn { border: 0; border-radius: 10px; padding: 9px 16px; font-size: 14px; cursor: pointer; color: #fff; background: #6366F1; white-space: nowrap; }
.btn.dark { background: #334155; }
.btn:disabled { opacity: .4; cursor: default; }
.clist { display: flex; align-items: center; gap: 8px; overflow-x: auto; }
#contacts { display: flex; gap: 8px; align-items: center; }
.contact { display: flex; align-items: center; gap: 8px; background: #0F172A; border-radius: 12px; padding: 6px; cursor: pointer; flex-shrink: 0; max-width: 240px; }
.contact:hover { background: #1a2538; }
.av { width: 30px; height: 30px; border-radius: 50%; color: #fff; font-weight: 700; font-size: 13px; display: flex; align-items: center; justify-content: center; flex-shrink: 0; }
.ci { min-width: 0; }
.cn { font-size: 13px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.ca { font-size: 11px; color: #64748B; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.x { background: none; border: 0; color: #475569; font-size: 14px; cursor: pointer; padding: 2px 6px; }
.x:hover { color: #F87171; }
.nocontacts { font-size: 12px; color: #64748B; }
#log { flex: 1; overflow-y: auto; padding: 16px 20px; display: flex; flex-direction: column; }
.chatwrap { width: 100%; max-width: 980px; margin: 0 auto; display: flex; flex-direction: column; }
.empty { margin: auto; text-align: center; color: #475569; font-size: 14px; line-height: 1.6; padding: 30px; }
.sys { align-self: center; font-size: 12px; color: #94A3B8; background: #1E293B; padding: 6px 12px; border-radius: 14px; margin: 8px 0; }
.mrow { display: flex; flex-direction: column; margin: 3px 0; }
.mrow.mine { align-items: flex-end; }
.mrow.theirs { align-items: flex-start; }
.who { font-size: 12px; color: #A5B4FC; margin: 0 0 3px 4px; }
.bub { max-width: 60%; padding: 10px 14px; border-radius: 18px; font-size: 15px; line-height: 1.4; white-space: pre-wrap; word-wrap: break-word; }
.mine .bub { background: #7C3AED; color: #fff; border-bottom-right-radius: 5px; }
.theirs .bub { background: #1E293B; border-bottom-left-radius: 5px; }
.bub.media { padding: 4px; background: #1E293B; border-radius: 14px; }
.media img, .media video { max-width: 360px; max-height: 320px; border-radius: 12px; display: block; }
.bub a { color: inherit; }
.tm { font-size: 11px; color: #64748B; margin: 2px 4px 0; }
.inbar { background: #0B1120; padding: 12px 16px; display: flex; justify-content: center; }
.inwrap { width: 100%; max-width: 980px; display: flex; gap: 10px; align-items: center; }
#text { flex: 1; background: #1E293B; border-radius: 24px; padding: 12px 18px; font-size: 15px; }
.round { width: 48px; height: 48px; border-radius: 50%; border: 0; color: #fff; font-size: 18px; cursor: pointer; flex-shrink: 0; }
.round:disabled { opacity: .4; cursor: default; }
.attach { background: #334155; }
.send { background: #6366F1; }
#file { display: none; }
#toast { position: fixed; bottom: 90px; left: 50%; transform: translateX(-50%); background: #1E293B; color: #F1F5F9; padding: 10px 16px; border-radius: 12px; font-size: 14px; display: none; z-index: 9; box-shadow: 0 10px 30px rgba(0,0,0,.4); }
#drop { position: absolute; inset: 0; display: none; align-items: center; justify-content: center; background: rgba(99,102,241,.25); border: 3px dashed #818CF8; font-size: 22px; font-weight: 700; color: #E0E7FF; z-index: 20; pointer-events: none; }
.app.dragging #drop { display: flex; }
</style>
</head>
<body>
<div class="app" id="app">
  <header>
    <div class="dot" id="dot"></div>
    <div class="hinfo">
      <div id="tvPeer">Нет собеседника</div>
      <div id="tvStatus">Не подключено</div>
    </div>
    <div class="spacer"></div>
    <div class="sig" title="Качество Wi-Fi соединения этого ПК (только Linux)">
      <div class="bars" id="bars"><i style="height:4px"></i><i style="height:7px"></i><i style="height:10px"></i><i style="height:14px"></i></div>
      <span id="sigText">—</span>
    </div>
    <div class="localip">IP ПК: <span id="localIp">...</span>:5000</div>
    <button class="gear" onclick="toggleSettings()" title="Настройки">⚙</button>
  </header>

  <div id="settings">
    <div class="bar">
      <div class="grp"><span class="glbl">Имя</span><input id="myName" maxlength="20" placeholder="Как вас называть"></div>
      <div class="grp">
        <span class="glbl">Связь</span>
        <button class="tog" id="tWifi" onclick="pick('transport','wifi')">Wi-Fi</button>
        <button class="tog" id="tBt" onclick="pick('transport','bt')">Bluetooth</button>
      </div>
      <div class="grp">
        <span class="glbl">Роль</span>
        <button class="tog" id="rHost" onclick="pick('role','host')">Хост</button>
        <button class="tog" id="rClient" onclick="pick('role','client')">Клиент</button>
      </div>
      <div class="grp">
        <div class="field" id="fIp"><input id="ip" placeholder="IP хоста (пусто = 192.168.43.1)"></div>
        <div class="field" id="fMac"><input id="mac" placeholder="MAC телефона"></div>
        <div class="field" id="fCh"><input id="channel" placeholder="Канал (необязательно)"></div>
        <div class="field" id="fName"><input id="cName" placeholder="Имя контакта"></div>
      </div>
      <div class="grp">
        <button class="btn dark" id="btnSave" style="display:none" onclick="saveContact()">Сохранить контакт</button>
        <button class="btn" id="btnConnect" onclick="connect()">Ждать подключения</button>
        <button class="btn dark" id="btnDisconnect" onclick="disconnect()" disabled>Отключить</button>
      </div>
    </div>
    <div class="clist">
      <span class="glbl">Контакты</span>
      <div id="contacts"></div>
    </div>
  </div>

  <div id="log">
    <div class="chatwrap" id="chatwrap">
      <div class="empty" id="empty">Пока тихо.<br>Выберите связь и роль в настройках (⚙) и нажмите «Ждать подключения» или «Подключиться».</div>
    </div>
  </div>

  <div class="inbar">
    <div class="inwrap">
      <button class="round attach" id="btnAttach" onclick="attachClick()" title="Отправить файл, фото или видео">📎</button>
      <input type="file" id="file" accept="*/*" multiple>
      <input id="text" placeholder="Сообщение..." autocomplete="off">
      <button class="round send" id="btnSend" onclick="send()" disabled>➤</button>
    </div>
  </div>

  <div id="drop">Отпустите файл, чтобы отправить</div>
</div>
<div id="toast"></div>

<script>
const $ = id => document.getElementById(id);
const CK = 'offichat.contacts', NK = 'offichat.name';
const MAX = 15 * 1024 * 1024;

let transport = 'wifi', role = 'host';
let since = 0, lastState = '', peer = '', curState = 'idle';
let contacts = [];
try { contacts = JSON.parse(localStorage.getItem(CK) || '[]'); } catch (e) { contacts = []; }

$('myName').value = localStorage.getItem(NK) || '';
$('myName').addEventListener('change', () => localStorage.setItem(NK, $('myName').value.trim()));
$('text').addEventListener('keydown', e => { if (e.key === 'Enter') send(); });
$('ip').addEventListener('keydown', e => { if (e.key === 'Enter') connect(); });
$('file').addEventListener('change', e => {
  for (const f of e.target.files) sendFile(f);
  e.target.value = '';
});

function attachClick() {
  if (curState !== 'connected') {
    toast('Сначала подключитесь, потом отправляйте файлы');
    return;
  }
  $('file').click();
}

let dragDepth = 0;
const hasFiles = e => e.dataTransfer && [...e.dataTransfer.types].includes('Files');
window.addEventListener('dragenter', e => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth++;
  $('app').classList.add('dragging');
});
window.addEventListener('dragover', e => {
  if (hasFiles(e)) e.preventDefault();
});
window.addEventListener('dragleave', () => {
  dragDepth--;
  if (dragDepth <= 0) {
    dragDepth = 0;
    $('app').classList.remove('dragging');
  }
});
window.addEventListener('drop', e => {
  e.preventDefault();
  dragDepth = 0;
  $('app').classList.remove('dragging');
  const files = e.dataTransfer && e.dataTransfer.files;
  if (!files || !files.length) return;
  if (curState !== 'connected') {
    toast('Сначала подключитесь, потом отправляйте файлы');
    return;
  }
  for (const f of files) sendFile(f);
});

function myLabel() { return $('myName').value.trim() || 'Вы'; }
function toggleSettings() { $('settings').classList.toggle('hidden'); }

function toast(msg) {
  const t = $('toast');
  t.textContent = msg;
  t.style.display = 'block';
  clearTimeout(t._t);
  t._t = setTimeout(() => t.style.display = 'none', 2600);
}

function pick(group, value) {
  if (group === 'transport') transport = value; else role = value;
  $('tWifi').classList.toggle('on', transport === 'wifi');
  $('tBt').classList.toggle('on', transport === 'bt');
  $('rHost').classList.toggle('on', role === 'host');
  $('rClient').classList.toggle('on', role === 'client');
  updateForm();
}

function updateForm() {
  const c = role === 'client';
  $('fIp').classList.toggle('show', transport === 'wifi' && c);
  $('fMac').classList.toggle('show', transport === 'bt' && c);
  $('fCh').classList.toggle('show', transport === 'bt' && c);
  $('fName').classList.toggle('show', c);
  $('btnSave').style.display = c ? 'inline-block' : 'none';
  $('btnConnect').textContent = c ? 'Подключиться' : 'Ждать подключения';
}

async function post(url, body) {
  const r = await fetch(url, {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify(body || {})
  });
  if (!r.ok) toast(await r.text());
  return r.ok;
}

async function connect() {
  const mac = $('mac').value.trim().toUpperCase();
  if (transport === 'bt' && role === 'client' && !mac) {
    toast('Введите MAC телефона');
    return;
  }
  await post('/api/connect', {
    transport: transport,
    role: role,
    ip: $('ip').value.trim(),
    mac: mac,
    channel: parseInt($('channel').value, 10) || 0
  });
}

function disconnect() { post('/api/disconnect'); }

async function send() {
  const input = $('text');
  const text = input.value.trim();
  if (!text) return;
  input.value = '';
  const ok = await post('/api/send', {text: text});
  if (!ok) input.value = text;
}

function sendFile(f) {
  if (!f) return;
  if (f.size > MAX) { toast(f.name + ': больше 15 МБ'); return; }
  toast('Отправляем ' + f.name + '...');
  const r = new FileReader();
  r.onload = async () => {
    const b64 = r.result.split(',')[1];
    await post('/api/sendfile', {name: f.name, data: b64});
  };
  r.onerror = () => toast('Не удалось прочитать файл');
  r.readAsDataURL(f);
}

function saveContacts() { localStorage.setItem(CK, JSON.stringify(contacts)); }

function matchContact(p) {
  if (!p) return null;
  const up = p.toUpperCase();
  return contacts.find(c => c.addr && up.indexOf(c.addr.toUpperCase()) === 0) || null;
}

function peerDisplay() {
  const c = matchContact(peer);
  return c ? c.name : (peer || 'Собеседник');
}

function saveContact() {
  const addr = transport === 'bt'
    ? $('mac').value.trim().toUpperCase()
    : ($('ip').value.trim() || '192.168.43.1');
  if (!addr) { toast('Сначала введите адрес'); return; }
  const name = $('cName').value.trim() || addr;
  const item = {name: name, transport: transport, addr: addr};
  const idx = contacts.findIndex(c => c.addr === addr);
  if (idx >= 0) contacts[idx] = item; else contacts.push(item);
  saveContacts();
  renderContacts();
  toast('Контакт «' + name + '» сохранён');
}

function fillFromContact(c) {
  pick('transport', c.transport);
  pick('role', 'client');
  if (c.transport === 'bt') $('mac').value = c.addr; else $('ip').value = c.addr;
  $('cName').value = c.name;
  toast('Выбран: ' + c.name + '. Нажмите «Подключиться»');
}

function hueOf(name) {
  let h = 0;
  for (const ch of name) h = (h * 31 + ch.charCodeAt(0)) % 360;
  return h;
}

function renderContacts() {
  const box = $('contacts');
  box.innerHTML = '';
  if (!contacts.length) {
    const h = document.createElement('span');
    h.className = 'nocontacts';
    h.textContent = 'Пока пусто. В режиме «Клиент» введите адрес и имя, затем «Сохранить контакт».';
    box.appendChild(h);
    return;
  }
  contacts.forEach((c, i) => {
    const row = document.createElement('div');
    row.className = 'contact';
    row.onclick = () => fillFromContact(c);

    const av = document.createElement('div');
    av.className = 'av';
    av.style.background = 'hsl(' + hueOf(c.name) + ', 55%, 45%)';
    av.textContent = (c.name.trim()[0] || '?').toUpperCase();

    const ci = document.createElement('div');
    ci.className = 'ci';
    const cn = document.createElement('div');
    cn.className = 'cn';
    cn.textContent = c.name;
    const ca = document.createElement('div');
    ca.className = 'ca';
    ca.textContent = (c.transport === 'bt' ? 'Bluetooth · ' : 'Wi-Fi · ') + c.addr;
    ci.appendChild(cn);
    ci.appendChild(ca);

    const x = document.createElement('button');
    x.className = 'x';
    x.textContent = '✕';
    x.onclick = e => {
      e.stopPropagation();
      contacts.splice(i, 1);
      saveContacts();
      renderContacts();
    };

    row.appendChild(av);
    row.appendChild(ci);
    row.appendChild(x);
    box.appendChild(row);
  });
}

function addBubble(m) {
  const wrap = $('chatwrap');
  const empty = $('empty');
  if (empty) empty.remove();

  if (m.from === 'system') {
    const s = document.createElement('div');
    s.className = 'sys';
    s.textContent = m.text;
    wrap.appendChild(s);
    return;
  }

  const mine = m.from === 'me';
  const row = document.createElement('div');
  row.className = 'mrow ' + (mine ? 'mine' : 'theirs');

  if (!mine) {
    const who = document.createElement('div');
    who.className = 'who';
    who.textContent = peerDisplay();
    row.appendChild(who);
  }

  const b = document.createElement('div');
  b.className = 'bub';
  if (m.kind === 'image' && m.url) {
    b.classList.add('media');
    const img = document.createElement('img');
    img.src = m.url;
    img.alt = m.text;
    img.onclick = () => window.open(m.url, '_blank');
    img.style.cursor = 'zoom-in';
    b.appendChild(img);
  } else if (m.kind === 'video' && m.url) {
    b.classList.add('media');
    const v = document.createElement('video');
    v.src = m.url;
    v.controls = true;
    b.appendChild(v);
  } else if (m.kind === 'file' && m.url) {
    const a = document.createElement('a');
    a.href = m.url;
    a.download = m.text;
    a.textContent = '📄 ' + m.text;
    b.appendChild(a);
  } else {
    b.textContent = m.text;
  }

  const t = document.createElement('div');
  t.className = 'tm';
  t.textContent = (mine ? myLabel() + ' · ' : '') + m.time;
  row.appendChild(b);
  row.appendChild(t);
  wrap.appendChild(row);
}

function setBars(level) {
  const n = level < 0 ? 0 : level >= 75 ? 4 : level >= 50 ? 3 : level >= 25 ? 2 : 1;
  document.querySelectorAll('#bars i').forEach((el, i) => el.classList.toggle('on', i < n));
}

async function pollSignal() {
  try {
    const r = await fetch('/api/signal');
    const s = await r.json();
    if (s.level < 0 || transport === 'bt') {
      $('sigText').textContent = '—';
      setBars(-1);
    } else {
      $('sigText').textContent = s.level + '%';
      setBars(s.level);
    }
  } catch (e) {}
}

async function poll() {
  try {
    const r = await fetch('/api/state?since=' + since);
    const s = await r.json();

    peer = s.peer || '';
    curState = s.state;
    $('localIp').textContent = s.localIp;
    $('tvStatus').textContent = s.status;
    $('tvPeer').textContent = s.state === 'connected' ? peerDisplay() : 'Нет собеседника';
    $('dot').className = 'dot ' + s.state;
    $('btnDisconnect').disabled = s.state === 'idle';
    $('btnConnect').disabled = s.state !== 'idle';
    $('btnSend').disabled = s.state !== 'connected';

    if (s.state !== lastState) {
      lastState = s.state;
      renderContacts();
    }

    if (s.messages.length) {
      s.messages.forEach(m => { since = m.id + 1; addBubble(m); });
      $('log').scrollTop = $('log').scrollHeight;
    }
  } catch (e) {
    $('tvStatus').textContent = 'Нет связи с сервером';
    $('dot').className = 'dot';
  }
}

pick('transport', 'wifi');
pick('role', 'host');
renderContacts();
poll();
pollSignal();
setInterval(poll, 500);
setInterval(pollSignal, 2000);
</script>
</body>
</html>`
