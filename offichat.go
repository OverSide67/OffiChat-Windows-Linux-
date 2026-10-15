// OffiChat desktop/web edition with logic ported from the Android Kotlin files.
// Copyright 2026 OverSide67; SPDX-License-Identifier: Apache-2.0
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
)

const (
	webAddr      = "127.0.0.1:8080"
	tcpPort      = "5000"
	maxFileBytes = 15 * 1024 * 1024
)

type Profile struct {
	Name      string `json:"name"`
	Transport string `json:"transport"` // wifi or bt
	Role      string `json:"role"`      // host or client
	Addr      string `json:"addr"`
}
type Message struct {
	ID   int    `json:"id"`
	From string `json:"from"`
	Kind string `json:"kind"`
	Text string `json:"text"`
	Path string `json:"path"`
	Time string `json:"time"`
}
type ConnectRequest struct {
	Profile   Profile `json:"profile"`
	Transport string  `json:"transport"`
	Role      string  `json:"role"`
	IP        string  `json:"ip"`
	MAC       string  `json:"mac"`
	Channel   int     `json:"channel"`
}
type diskData struct {
	Name      string               `json:"name"`
	Profiles  []Profile            `json:"profiles"`
	Current   *Profile             `json:"current,omitempty"`
	Histories map[string][]Message `json:"histories"`
}
type app struct {
	mu                  sync.Mutex
	writeMu             sync.Mutex
	data                diskData
	dataPath            string
	recvDir             string
	conn                net.Conn
	listener            net.Listener
	gen                 int
	state, status, peer string
	messages            []Message
	historyKey          string
}

var st *app

func newApp() *app {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, "OffiChat")
	_ = os.MkdirAll(root, 0755)
	a := &app{dataPath: filepath.Join(root, "data.json"), recvDir: filepath.Join(root, "received"), state: "idle", status: "Не подключено"}
	_ = os.MkdirAll(a.recvDir, 0755)
	a.data = diskData{Profiles: []Profile{}, Histories: map[string][]Message{}}
	if b, err := os.ReadFile(a.dataPath); err == nil {
		_ = json.Unmarshal(b, &a.data)
	}
	if a.data.Profiles == nil {
		a.data.Profiles = []Profile{}
	}
	if a.data.Histories == nil {
		a.data.Histories = map[string][]Message{}
	}
	return a
}
func (a *app) persistLocked() {
	b, e := json.MarshalIndent(a.data, "", "  ")
	if e == nil {
		_ = os.WriteFile(a.dataPath, b, 0600)
	}
}
func (a *app) addLocked(from, kind, text, path string) {
	m := Message{ID: len(a.messages), From: from, Kind: kind, Text: text, Path: path, Time: time.Now().Format("15:04")}
	a.messages = append(a.messages, m)
	if a.historyKey != "" {
		h := a.data.Histories[a.historyKey]
		h = append(h, m)
		if len(h) > 5000 {
			h = h[len(h)-5000:]
		}
		for i := range h {
			h[i].ID = i
		}
		a.data.Histories[a.historyKey] = h
		a.persistLocked()
	}
}
func (a *app) add(from, kind, text, path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.addLocked(from, kind, text, path)
}
func (a *app) setState(state, status, peer string) {
	a.mu.Lock()
	a.state, a.status, a.peer = state, status, peer
	a.mu.Unlock()
}
func (a *app) disconnect() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gen++
	if a.listener != nil {
		_ = a.listener.Close()
		a.listener = nil
	}
	if a.conn != nil {
		_ = a.conn.Close()
		a.conn = nil
	}
	a.state = "idle"
	a.status = "Не подключено"
	a.peer = ""
	return a.gen
}
func (a *app) connect(req ConnectRequest) { gen := a.disconnect(); go a.run(req, gen) }
func (a *app) run(req ConnectRequest, gen int) {
	p := req.Profile
	if p.Transport == "" {
		p.Transport = req.Transport
	}
	if p.Role == "" {
		p.Role = req.Role
	}
	if p.Addr == "" {
		p.Addr = req.IP
	}
	if p.Addr == "" {
		p.Addr = req.MAC
	}
	if p.Transport == "bt" {
		a.fail(gen, "Bluetooth-подключение с ПК зависит от ОС; в этой сборке доступно Wi-Fi. Используйте Wi-Fi-клиент/хост.")
		return
	}
	var c net.Conn
	var err error
	peer := ""
	if p.Role == "host" {
		ln, e := net.Listen("tcp", "0.0.0.0:"+tcpPort)
		if e != nil {
			a.fail(gen, e.Error())
			return
		}
		a.mu.Lock()
		if gen != a.gen {
			a.mu.Unlock()
			_ = ln.Close()
			return
		}
		a.listener = ln
		a.state = "waiting"
		a.status = "Wi-Fi: ждём подключения на " + localIP() + ":" + tcpPort
		a.mu.Unlock()
		c, err = ln.Accept()
		_ = ln.Close()
		a.mu.Lock()
		if a.listener == ln {
			a.listener = nil
		}
		a.mu.Unlock()
		if c != nil {
			peer = c.RemoteAddr().String()
		}
	} else {
		host := strings.TrimSpace(p.Addr)
		if host == "" {
			host = "192.168.43.1"
		}
		if h, _, e := net.SplitHostPort(host); e == nil {
			host = h
		}
		target := net.JoinHostPort(strings.Trim(host, "[]"), tcpPort)
		a.setState("connecting", "Подключаемся к "+target+"...", "")
		c, err = net.DialTimeout("tcp", target, 10*time.Second)
		peer = host
	}
	if err != nil {
		a.fail(gen, err.Error())
		return
	}
	a.mu.Lock()
	if gen != a.gen {
		a.mu.Unlock()
		_ = c.Close()
		return
	}
	a.conn = c
	a.state = "connected"
	a.status = "Подключено"
	a.peer = peer
	key := p.Name
	if key == "" {
		key = peer
	}
	a.historyKey = key
	a.messages = append([]Message(nil), a.data.Histories[key]...)
	for i := range a.messages {
		a.messages[i].ID = i
	}
	a.addLocked("system", "system", "Подключено: "+peer, "")
	a.mu.Unlock()
	a.readLoop(c, gen)
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
func (a *app) readLoop(c net.Conn, gen int) {
	s := bufio.NewScanner(c)
	s.Buffer(make([]byte, 64*1024), maxFileBytes*2+1024)
	for s.Scan() {
		line := s.Text()
		switch {
		case strings.HasPrefix(line, "V|"):
			a.add("peer", "voice", strings.TrimPrefix(line, "V|"), "")
		case strings.HasPrefix(line, "F|"):
			a.receiveFile(line)
		default:
			a.add("peer", "text", line, "")
		}
	}
	reason := "Соединение разорвано"
	if e := s.Err(); e != nil {
		reason = "Обрыв связи: " + e.Error()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if gen == a.gen && a.conn == c {
		a.conn = nil
		a.state = "idle"
		a.status = reason
		a.peer = ""
		a.addLocked("system", "system", reason, "")
	}
}
func (a *app) sendLine(line string) error {
	a.mu.Lock()
	c := a.conn
	a.mu.Unlock()
	if c == nil {
		return errors.New("нет подключения")
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_, e := io.WriteString(c, line+"\n")
	return e
}
func (a *app) sendText(text string) error {
	text = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(text, "\n", " "), "\r", " "))
	if text == "" {
		return errors.New("пустое сообщение")
	}
	if e := a.sendLine(text); e != nil {
		return e
	}
	a.add("me", "text", text, "")
	return nil
}
func safeName(n string) string {
	n = filepath.Base(strings.TrimSpace(n))
	n = strings.Map(func(r rune) rune {
		if r == '|' || r == '\n' || r == '\r' || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, n)
	if n == "" || n == "." {
		return "file"
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
func (a *app) saveFile(name string, b []byte) (string, error) {
	if len(b) > maxFileBytes {
		return "", errors.New("файл больше 15 МБ")
	}
	if e := os.MkdirAll(a.recvDir, 0755); e != nil {
		return "", e
	}
	n := time.Now().Format("20060102_150405.000") + "_" + safeName(name)
	e := os.WriteFile(filepath.Join(a.recvDir, n), b, 0644)
	return n, e
}
func (a *app) sendFile(name string, b []byte) error {
	if len(b) == 0 {
		return errors.New("пустой файл")
	}
	if len(b) > maxFileBytes {
		return errors.New("файл больше 15 МБ")
	}
	safe := safeName(name)
	line := "F|" + safe + "|" + base64.StdEncoding.EncodeToString(b)
	if e := a.sendLine(line); e != nil {
		return e
	}
	saved, e := a.saveFile("sent_"+safe, b)
	if e != nil {
		return e
	}
	a.add("me", kindOf(safe), safe, "/files/"+saved)
	return nil
}
func (a *app) receiveFile(line string) {
	p := strings.SplitN(line, "|", 3)
	if len(p) != 3 {
		return
	}
	b, e := base64.StdEncoding.DecodeString(p[2])
	if e != nil {
		a.add("system", "system", "Файл повреждён при приёме", "")
		return
	}
	if len(b) > maxFileBytes {
		a.add("system", "system", "Файл больше 15 МБ", "")
		return
	}
	name := safeName(p[1])
	saved, e := a.saveFile(name, b)
	if e != nil {
		a.add("system", "system", "Не удалось сохранить файл: "+e.Error(), "")
		return
	}
	a.add("peer", kindOf(name), name, "/files/"+saved)
}
func localIP() string {
	cs, e := net.Interfaces()
	if e == nil {
		for _, it := range cs {
			as, _ := it.Addrs()
			for _, ad := range as {
				ip, _, _ := net.ParseCIDR(ad.String())
				if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
					return ip.String()
				}
			}
		}
	}
	return "127.0.0.1"
}
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, maxFileBytes*2+1024*1024)).Decode(v)
}
func (a *app) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, pageHTML)
	})
	m.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		msgs := []Message{}
		for _, x := range a.messages {
			if x.ID >= since {
				msgs = append(msgs, x)
			}
		}
		jsonOut(w, map[string]any{"state": a.state, "status": a.status, "peer": a.peer, "localIp": localIP(), "messages": msgs, "name": a.data.Name, "profiles": a.data.Profiles, "current": a.data.Current})
	})
	m.HandleFunc("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		var q ConnectRequest
		if e := decodeBody(r, &q); e != nil {
			http.Error(w, "неверный запрос", 400)
			return
		}
		if q.Profile.Name == "" {
			q.Profile = Profile{Name: "Wi-Fi", Transport: q.Transport, Role: q.Role, Addr: q.IP}
			if q.Profile.Role == "" {
				q.Profile.Role = "client"
			}
		}
		a.mu.Lock()
		a.data.Current = &q.Profile
		a.persistLocked()
		a.mu.Unlock()
		a.connect(q)
		jsonOut(w, map[string]bool{"ok": true})
	})
	m.HandleFunc("/api/disconnect", func(w http.ResponseWriter, r *http.Request) { a.disconnect(); jsonOut(w, map[string]bool{"ok": true}) })
	m.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Text string `json:"text"`
		}
		if e := decodeBody(r, &q); e != nil {
			http.Error(w, "неверный запрос", 400)
			return
		}
		if e := a.sendText(q.Text); e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		jsonOut(w, map[string]bool{"ok": true})
	})
	m.HandleFunc("/api/sendfile", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Name string `json:"name"`
			Data string `json:"data"`
		}
		if e := decodeBody(r, &q); e != nil {
			http.Error(w, "файл слишком большой или запрос неверен", 413)
			return
		}
		b, e := base64.StdEncoding.DecodeString(q.Data)
		if e != nil {
			http.Error(w, "неверные данные файла", 400)
			return
		}
		if e = a.sendFile(q.Name, b); e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		jsonOut(w, map[string]bool{"ok": true})
	})
	m.HandleFunc("/api/voice", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Data string `json:"data"`
		}
		if e := decodeBody(r, &q); e != nil {
			http.Error(w, "неверный запрос", 400)
			return
		}
		if q.Data == "" {
			http.Error(w, "пустой аудиофрагмент", 400)
			return
		}
		if e := a.sendLine("V|" + q.Data); e != nil {
			http.Error(w, e.Error(), 409)
			return
		}
		jsonOut(w, map[string]bool{"ok": true})
	})
	m.HandleFunc("/api/profile", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Action  string  `json:"action"`
			Profile Profile `json:"profile"`
			Index   int     `json:"index"`
		}
		if e := decodeBody(r, &q); e != nil {
			http.Error(w, "неверный запрос", 400)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		switch q.Action {
		case "save":
			p := q.Profile
			p.Name = strings.TrimSpace(p.Name)
			if p.Name == "" {
				http.Error(w, "Введите название", 400)
				return
			}
			if p.Transport != "bt" {
				p.Transport = "wifi"
			}
			if p.Role != "host" {
				p.Role = "client"
			}
			a.data.Profiles = append(a.data.Profiles, p)
			a.data.Current = &p
		case "select":
			if q.Index < 0 || q.Index >= len(a.data.Profiles) {
				http.Error(w, "контакт не найден", 404)
				return
			}
			p := a.data.Profiles[q.Index]
			a.data.Current = &p
		case "delete":
			if q.Index < 0 || q.Index >= len(a.data.Profiles) {
				http.Error(w, "контакт не найден", 404)
				return
			}
			a.data.Profiles = append(a.data.Profiles[:q.Index], a.data.Profiles[q.Index+1:]...)
		case "clearHistory":
			a.data.Histories = map[string][]Message{}
			a.messages = nil
		default:
			http.Error(w, "неизвестное действие", 400)
			return
		}
		a.persistLocked()
		jsonOut(w, map[string]bool{"ok": true})
	})
	m.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Name string `json:"name"`
		}
		if e := decodeBody(r, &q); e != nil {
			http.Error(w, "неверный запрос", 400)
			return
		}
		a.mu.Lock()
		a.data.Name = strings.TrimSpace(q.Name)
		a.persistLocked()
		a.mu.Unlock()
		jsonOut(w, map[string]bool{"ok": true})
	})
	m.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(a.recvDir))))
	return m
}
func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		c = exec.Command("open", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}
func main() {
	st = newApp()
	url := "http://localhost:8080"
	fmt.Println("OffiChat запущен:", url)
	fmt.Println("IP для подключения:", localIP(), "порт", tcpPort)
	fmt.Println("Полученные файлы:", st.recvDir)
	go func() { time.Sleep(300 * time.Millisecond); openBrowser(url) }()
	srv := &http.Server{Addr: webAddr, Handler: st.routes(), ReadHeaderTimeout: 5 * time.Second}
	if e := srv.ListenAndServe(); e != nil {
		fmt.Println("Ошибка сервера:", e)
	}
}

const pageHTML = `<!doctype html><html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>OffiChat</title><style>
*{box-sizing:border-box}body{margin:0;background:#0f172a;color:#f1f5f9;font:15px system-ui,Segoe UI,sans-serif}.app{height:100dvh;display:flex;flex-direction:column}header{padding:12px 16px;background:#0b1120;border-bottom:1px solid #263449;display:flex;gap:12px;align-items:center}.dot{width:10px;height:10px;border-radius:50%;background:#64748b}.dot.connected{background:#22c55e}.dot.waiting,.dot.connecting{background:#facc15}.muted{color:#94a3b8;font-size:12px}.grow{flex:1}button,input,select{border:0;border-radius:9px;padding:10px;background:#1e293b;color:#f1f5f9;font:inherit}button{cursor:pointer;background:#4f46e5}button.secondary{background:#334155}button.danger{background:#9f1239}input{min-width:0;border:1px solid #334155;width:100%}.panel{padding:12px 16px;background:#111c30;display:flex;flex-wrap:wrap;gap:8px;align-items:center}.panel input{width:auto;flex:1;min-width:130px}.panel select{max-width:140px}.contacts{display:flex;gap:7px;overflow:auto;width:100%;padding-top:4px}.contact{white-space:nowrap;background:#0f172a;border:1px solid #334155;font-size:13px}.contact.active{border-color:#818cf8;background:#312e81}#chat{flex:1;overflow:auto;padding:16px;display:flex;flex-direction:column;gap:9px}.bubble{max-width:min(78%,680px);padding:10px 12px;border-radius:14px;background:#1e293b;overflow-wrap:anywhere;align-self:flex-start}.bubble.me{align-self:flex-end;background:#3730a3}.bubble.system{align-self:center;background:transparent;color:#94a3b8;font-size:12px}.time{font-size:10px;color:#a5b4fc;margin-top:5px}.composer{display:flex;gap:8px;padding:12px;background:#0b1120}.composer input{flex:1}.file{max-width:100%}#settings{display:flex;gap:8px;flex-wrap:wrap;align-items:center;width:100%}.hidden{display:none!important}@media(max-width:600px){header{padding:10px}.panel{padding:9px}.bubble{max-width:90%}.composer{padding:8px}}
</style></head><body><div class="app"><header><span id="dot" class="dot"></span><div><b id="status">Не подключено</b><div class="muted" id="peer">IP: …</div></div><div class="grow"></div><button class="secondary" onclick="toggleSettings()">⚙</button></header><section class="panel" id="settings"><input id="myName" placeholder="Ваше имя" maxlength="60"><button class="secondary" onclick="saveName()">Сохранить имя</button><select id="transport" onchange="transportChanged()"><option value="wifi">Wi-Fi</option><option value="bt">Bluetooth</option></select><select id="role" onchange="roleChanged()"><option value="client">Клиент</option><option value="host">Хост</option></select><input id="pname" placeholder="Название контакта"><input id="addr" placeholder="IP хоста, например 192.168.43.1"><button onclick="saveProfile()">＋ Контакт</button><button onclick="connect()">Подключить</button><button class="danger" onclick="disconnect()">Отключить</button><div class="contacts" id="contacts"></div><button class="secondary" onclick="clearHistory()">Очистить историю</button><span class="muted">TCP 5000 · файлы до 15 МБ</span></section><main id="chat"></main><div class="composer"><input id="text" placeholder="Сообщение…" autocomplete="off"><button class="secondary" onclick="document.getElementById('file').click()">📎</button><input class="hidden" id="file" type="file"><button id="talk" class="secondary" title="Зажмите для голосовой связи">🎙</button><button onclick="send()">Отправить</button></div></div><script>
let last=-1, state='idle', profiles=[], current=null, audioCtx=null, micStream=null, processor=null, voiceOn=false, nextPlay=0;const $=id=>document.getElementById(id);
async function api(path,data){let r=await fetch(path,{method:data?'POST':'GET',headers:data?{'Content-Type':'application/json'}:{},body:data?JSON.stringify(data):undefined});let t=await r.text();if(!r.ok)throw Error(t);try{return JSON.parse(t)}catch{return t}}
function toggleSettings(){$('settings').classList.toggle('hidden')};function roleChanged(){$('addr').classList.toggle('hidden',$('role').value==='host')};function transportChanged(){$('addr').placeholder=$('transport').value==='bt'?'MAC Bluetooth (не поддерживается в этой сборке)':'IP хоста, например 192.168.43.1'}
async function saveName(){try{await api('/api/settings',{name:$('myName').value});}catch(e){alert(e.message)}}
async function saveProfile(){try{let p={name:$('pname').value.trim(),transport:$('transport').value,role:$('role').value,addr:$('role').value==='host'?'':$('addr').value.trim()};await api('/api/profile',{action:'save',profile:p});$('pname').value='';await refresh(true)}catch(e){alert(e.message)}}
async function selectProfile(i){try{await api('/api/profile',{action:'select',index:i});await refresh(true);let p=profiles[i];$('transport').value=p.transport;$('role').value=p.role;$('addr').value=p.addr;roleChanged();$('pname').value=p.name}catch(e){alert(e.message)}}
async function deleteProfile(i){if(!confirm('Удалить контакт?'))return;try{await api('/api/profile',{action:'delete',index:i});await refresh(true)}catch(e){alert(e.message)}}
function renderContacts(){let box=$('contacts');box.innerHTML='';profiles.forEach((p,i)=>{let b=document.createElement('button');b.className='contact'+(current&&current.name===p.name?' active':'');b.textContent=p.name+' · '+(p.transport==='bt'?'Bluetooth':'Wi-Fi')+' · '+(p.role==='host'?'Хост':p.addr||'Клиент');b.onclick=()=>selectProfile(i);b.oncontextmenu=e=>{e.preventDefault();deleteProfile(i)};box.appendChild(b)})}
async function connect(){try{let p=current||{name:$('pname').value||'Wi-Fi',transport:$('transport').value,role:$('role').value,addr:$('addr').value};await api('/api/connect',{profile:p});last=-1;await refresh(true)}catch(e){alert(e.message)}}async function disconnect(){await api('/api/disconnect');await refresh(true)}
async function send(){let t=$('text').value;if(!t.trim())return;try{await api('/api/send',{text:t});$('text').value='';await refresh()}catch(e){alert(e.message)}}$('text').addEventListener('keydown',e=>{if(e.key==='Enter')send()});
$('file').onchange=async e=>{let f=e.target.files[0];if(!f)return;if(f.size>15*1024*1024){alert('Максимум 15 МБ');e.target.value='';return}try{let data=await new Promise((res,rej)=>{let r=new FileReader();r.onload=()=>res(r.result.split(',')[1]);r.onerror=rej;r.readAsDataURL(f)});await api('/api/sendfile',{name:f.name,data});await refresh()}catch(err){alert(err.message)}e.target.value=''};
function renderMessage(m){if(m.kind==='voice'){playVoice(m.text);return}let d=document.createElement('div');d.className='bubble '+(m.from==='me'?'me':m.from==='system'?'system':'');if(m.kind==='image'&&m.path){let im=document.createElement('img');im.src=m.path;im.className='file';im.style.maxHeight='320px';d.appendChild(im)}else if(m.kind==='video'&&m.path){let v=document.createElement('video');v.src=m.path;v.controls=true;v.className='file';v.style.maxHeight='320px';d.appendChild(v)}else if(m.path){let a=document.createElement('a');a.href=m.path;a.textContent='📎 '+m.text;a.download=m.text;d.appendChild(a)}else d.appendChild(document.createTextNode(m.text));if(m.time){let tm=document.createElement('div');tm.className='time';tm.textContent=m.time;d.appendChild(tm)}$('chat').appendChild(d)}
async function refresh(reset=false){try{let s=await api('/api/state?since='+(reset?0:last+1));state=s.state;$('status').textContent=s.status;$('peer').textContent=(s.peer?'Собеседник: '+s.peer+' · ':'')+'IP этого ПК: '+s.localIp+':5000';$('dot').className='dot '+state;if(reset){$('chat').innerHTML='';last=-1}for(let m of s.messages){if(m.id>last){renderMessage(m);last=Math.max(last,m.id)}}$('chat').scrollTop=$('chat').scrollHeight;if(s.profiles){profiles=s.profiles;current=s.current||null;renderContacts()}if($('myName').value===''&&s.name)$('myName').value=s.name}catch(e){}}setInterval(()=>refresh(),700);refresh(true);roleChanged();
function playVoice(b64){try{audioCtx=audioCtx||new AudioContext({sampleRate:8000});let raw=atob(b64),buf=audioCtx.createBuffer(1,raw.length,8000),ch=buf.getChannelData(0);for(let i=0;i<raw.length;i++)ch[i]=mulawToLinear(raw.charCodeAt(i))/32768;let src=audioCtx.createBufferSource();src.buffer=buf;src.connect(audioCtx.destination);let start=Math.max(audioCtx.currentTime,nextPlay);src.start(start);nextPlay=start+buf.duration}catch(e){}}
// G.711 μ-law codec compatible with VoiceEngine.kt (8 kHz, mono, 320-byte/40 ms frames).
function linearToMulaw(sample){let s=Math.max(-32635,Math.min(32635,Math.round(sample))),sign=s<0?128:0;if(s<0)s=-s;s+=132;let exp=7,mask=0x4000;while(exp>0&&(s&mask)===0){exp--;mask>>=1}let mant=(s>>(exp+3))&15;return (~(sign|(exp<<4)|mant))&255}
function mulawToLinear(b){let u=(~b)&255,sign=u&128,exp=(u>>4)&7,mant=u&15,s=(((mant<<3)+132)<<exp)-132;return sign?-s:s}
async function startVoice(){if(state!=='connected'){alert('Сначала подключитесь');return}try{audioCtx=audioCtx||new AudioContext({sampleRate:8000});await audioCtx.resume();micStream=await navigator.mediaDevices.getUserMedia({audio:{channelCount:1,sampleRate:8000,echoCancellation:true}});let src=audioCtx.createMediaStreamSource(micStream);processor=audioCtx.createScriptProcessor(1024,1,1);processor.onaudioprocess=e=>{if(!voiceOn)return;let x=e.inputBuffer.getChannelData(0);let bytes=[],step=Math.max(1,Math.round(audioCtx.sampleRate/8000));for(let i=0;i<x.length;i+=step)bytes.push(linearToMulaw(x[i]*32767));if(bytes.length)fetch('/api/voice',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({data:btoa(String.fromCharCode(...bytes))})}).catch(()=>{})};src.connect(processor);processor.connect(audioCtx.destination);voiceOn=true;$('talk').textContent='🔴 Отпустить';}catch(e){alert('Микрофон недоступен: '+e.message);stopVoice()}}
function stopVoice(){voiceOn=false;if(processor){processor.disconnect();processor=null}if(micStream){micStream.getTracks().forEach(t=>t.stop());micStream=null}$('talk').textContent='🎙'}$('talk').onpointerdown=e=>{e.preventDefault();startVoice()};$('talk').onpointerup=stopVoice;$('talk').onpointercancel=stopVoice;
</script></body></html>`
