// airinput-tv: Android TV host port of airInput (https://github.com/DiegoCChumbi/airInput)
//
// Runs on the TV itself as the adb shell user (no root, no app install).
// Serves the airInput web client over HTTP and accepts gamepad input over a
// plain WebSocket. Each connected phone gets its own virtual HID gamepad
// created through /dev/uhid (the shell user is in the "uhid" group).
//
// Build:  GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -o airinput-tv .
// Run:    adb shell /data/local/tmp/airinput-tv
package main

import (
	"bufio"
	"crypto/sha1"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
)

//go:embed public
var publicFiles embed.FS

const listenPort = 3000

// ---------------------------------------------------------------------------
// UHID virtual gamepad
// ---------------------------------------------------------------------------

// HID report descriptor borrowed from scrcpy's hid_gamepad.c (proven to map
// correctly on Android): left stick X/Y + right stick Rx/Ry (16-bit, 0-65535),
// triggers Z/Rz (16-bit, 0-32767), 16 buttons, 4-bit hat switch for the dpad.
var gamepadReportDesc = []byte{
	0x05, 0x01, // Usage Page (Generic Desktop)
	0x09, 0x05, // Usage (Gamepad)
	0xA1, 0x01, // Collection (Application)
	0xA1, 0x00, // Collection (Physical)
	0x05, 0x01, // Usage Page (Generic Desktop)
	0x09, 0x30, // Usage (X)
	0x09, 0x31, // Usage (Y)
	0x09, 0x33, // Usage (Rx)
	0x09, 0x34, // Usage (Ry)
	0x15, 0x00, // Logical Minimum (0)
	0x27, 0xFF, 0xFF, 0x00, 0x00, // Logical Maximum (65535)
	0x75, 0x10, // Report Size (16)
	0x95, 0x04, // Report Count (4)
	0x81, 0x02, // Input (Data, Variable, Absolute)
	0x05, 0x01, // Usage Page (Generic Desktop)
	0x09, 0x32, // Usage (Z)
	0x09, 0x35, // Usage (Rz)
	0x15, 0x00, // Logical Minimum (0)
	0x26, 0xFF, 0x7F, // Logical Maximum (32767)
	0x75, 0x10, // Report Size (16)
	0x95, 0x02, // Report Count (2)
	0x81, 0x02, // Input (Data, Variable, Absolute)
	0x05, 0x09, // Usage Page (Buttons)
	0x19, 0x01, // Usage Minimum (1)
	0x29, 0x10, // Usage Maximum (16)
	0x15, 0x00, // Logical Minimum (0)
	0x25, 0x01, // Logical Maximum (1)
	0x95, 0x10, // Report Count (16)
	0x75, 0x01, // Report Size (1)
	0x81, 0x02, // Input (Data, Variable, Absolute)
	0x05, 0x01, // Usage Page (Generic Desktop)
	0x09, 0x39, // Usage (Hat switch)
	0x15, 0x01, // Logical Minimum (1)
	0x25, 0x08, // Logical Maximum (8)
	0x75, 0x04, // Report Size (4)
	0x95, 0x01, // Report Count (1)
	0x81, 0x42, // Input (Data, Variable, Absolute, Null State)
	0xC0, // End Collection
	0xC0, // End Collection
}

const (
	uhidEventGetReport      = 9
	uhidEventGetReportReply = 10
	uhidEventCreate2        = 11
	uhidEventInput2         = 12

	reportSize = 15 // 4x16-bit stick axes + 2x16-bit triggers + 16 buttons + hat
)

// Button bit positions. With a Gamepad application collection the kernel maps
// HID button N to evdev BTN_GAMEPAD+N-1, which Android's Generic.kl turns into
// BUTTON_A/B/X/Y/L1/R1/L2/R2/SELECT/START.
var buttonBits = map[string]uint{
	"A": 0, "B": 1, "X": 3, "Y": 4,
	"L": 6, "R": 7, "L2": 8, "R2": 9,
	"SELECT": 10, "START": 11,
}

// D-pad directions are a hat switch rather than buttons, so they index dpad[]
// instead of setting a button bit. Kept as a map, not a switch, so that these
// names and buttonBits together are the one machine-readable list of what a
// skin may name — TestSkins checks every layout against exactly this set.
var dpadIndex = map[string]int{
	"UP": 0, "DOWN": 1, "LEFT": 2, "RIGHT": 3,
}

// knownButton reports whether setButton would act on this name. A skin naming
// anything else is silently ignored at runtime, which is why it is worth
// failing the build over.
func knownButton(name string) bool {
	if _, ok := dpadIndex[name]; ok {
		return true
	}
	_, ok := buttonBits[name]
	return ok
}

type gamepad struct {
	fd *os.File

	mu       sync.Mutex
	axes     [4]uint16 // lx, ly, rx, ry
	triggers [2]uint16 // l2, r2
	buttons  uint16
	dpad     [4]bool // up, down, left, right
}

func newGamepad(name string) (*gamepad, error) {
	fd, err := os.OpenFile("/dev/uhid", os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/uhid: %w", err)
	}

	// struct uhid_event { u32 type; struct uhid_create2_req { u8 name[128];
	// u8 phys[64]; u8 uniq[64]; u16 rd_size; u16 bus; u32 vendor; u32 product;
	// u32 version; u32 country; u8 rd_data[]; } }
	buf := make([]byte, 280+len(gamepadReportDesc))
	binary.LittleEndian.PutUint32(buf[0:], uhidEventCreate2)
	copy(buf[4:4+127], name)
	binary.LittleEndian.PutUint16(buf[260:], uint16(len(gamepadReportDesc)))
	binary.LittleEndian.PutUint16(buf[262:], 0x03) // BUS_USB
	binary.LittleEndian.PutUint32(buf[264:], 0x0001)
	binary.LittleEndian.PutUint32(buf[268:], 0x0001)
	copy(buf[280:], gamepadReportDesc)

	if _, err := fd.Write(buf); err != nil {
		fd.Close()
		return nil, fmt.Errorf("UHID_CREATE2: %w", err)
	}

	g := &gamepad{fd: fd}
	g.axes = [4]uint16{0x8000, 0x8000, 0x8000, 0x8000} // sticks centered
	go g.drainEvents()
	g.sendReport()
	return g, nil
}

// drainEvents keeps the uhid event queue empty and answers GET_REPORT so the
// kernel never blocks waiting on us.
func (g *gamepad) drainEvents() {
	buf := make([]byte, 4380)
	for {
		n, err := g.fd.Read(buf)
		if err != nil {
			return
		}
		if n < 4 {
			continue
		}
		if binary.LittleEndian.Uint32(buf[0:]) == uhidEventGetReport {
			id := binary.LittleEndian.Uint32(buf[4:])
			reply := make([]byte, 12+reportSize)
			binary.LittleEndian.PutUint32(reply[0:], uhidEventGetReportReply)
			binary.LittleEndian.PutUint32(reply[4:], id)
			binary.LittleEndian.PutUint16(reply[8:], 0) // err
			binary.LittleEndian.PutUint16(reply[10:], reportSize)
			copy(reply[12:], g.buildReport())
			g.fd.Write(reply)
		}
	}
}

func (g *gamepad) buildReport() []byte {
	r := make([]byte, reportSize)
	for i, v := range g.axes {
		binary.LittleEndian.PutUint16(r[i*2:], v)
	}
	binary.LittleEndian.PutUint16(r[8:], g.triggers[0])
	binary.LittleEndian.PutUint16(r[10:], g.triggers[1])
	binary.LittleEndian.PutUint16(r[12:], g.buttons)
	r[14] = hatValue(g.dpad)
	return r
}

func hatValue(d [4]bool) byte {
	up, down, left, right := d[0], d[1], d[2], d[3]
	switch {
	case up && right:
		return 2
	case down && right:
		return 4
	case down && left:
		return 6
	case up && left:
		return 8
	case up:
		return 1
	case right:
		return 3
	case down:
		return 5
	case left:
		return 7
	}
	return 0 // outside logical range = null state = dpad released
}

func (g *gamepad) sendReport() {
	report := g.buildReport()
	buf := make([]byte, 6+reportSize)
	binary.LittleEndian.PutUint32(buf[0:], uhidEventInput2)
	binary.LittleEndian.PutUint16(buf[4:], reportSize)
	copy(buf[6:], report)
	g.fd.Write(buf)
}

func (g *gamepad) setButton(name string, pressed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if idx, ok := dpadIndex[name]; ok {
		g.dpad[idx] = pressed
	} else {
		bit, ok := buttonBits[name]
		if !ok {
			return
		}
		if pressed {
			g.buttons |= 1 << bit
		} else {
			g.buttons &^= 1 << bit
		}
		// L2/R2 double as analog triggers; some games only read the axis.
		if name == "L2" || name == "R2" {
			idx := 0
			if name == "R2" {
				idx = 1
			}
			if pressed {
				g.triggers[idx] = 32767
			} else {
				g.triggers[idx] = 0
			}
		}
	}
	g.sendReport()
}

func (g *gamepad) setAxis(name string, value float64) {
	idx := map[string]int{"lx": 0, "ly": 1, "rx": 2, "ry": 3}[name]
	if value > 1 {
		value = 1
	} else if value < -1 {
		value = -1
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.axes[idx] = uint16((value + 1) / 2 * 65535)
	g.sendReport()
}

// Closing the fd makes the kernel destroy the virtual device.
func (g *gamepad) close() { g.fd.Close() }

// ---------------------------------------------------------------------------
// Minimal WebSocket server (RFC 6455, text frames only)
// ---------------------------------------------------------------------------

type wsConn struct {
	conn net.Conn
	rw   *bufio.ReadWriter
	mu   sync.Mutex
}

func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return nil, errors.New("not a websocket request")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijack unsupported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	accept := base64.StdEncoding.EncodeToString(h[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, rw: rw}, nil
}

// readMessage returns the next complete text message payload.
func (ws *wsConn) readMessage() ([]byte, error) {
	var message []byte
	for {
		header := make([]byte, 2)
		if _, err := io.ReadFull(ws.rw, header); err != nil {
			return nil, err
		}
		fin := header[0]&0x80 != 0
		opcode := header[0] & 0x0F
		masked := header[1]&0x80 != 0
		length := uint64(header[1] & 0x7F)
		if length == 126 {
			ext := make([]byte, 2)
			if _, err := io.ReadFull(ws.rw, ext); err != nil {
				return nil, err
			}
			length = uint64(binary.BigEndian.Uint16(ext))
		} else if length == 127 {
			ext := make([]byte, 8)
			if _, err := io.ReadFull(ws.rw, ext); err != nil {
				return nil, err
			}
			length = binary.BigEndian.Uint64(ext)
		}
		if length > 1<<20 {
			return nil, errors.New("frame too large")
		}
		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(ws.rw, maskKey[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(ws.rw, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}
		switch opcode {
		case 0x8: // close
			return nil, io.EOF
		case 0x9: // ping -> pong
			ws.writeFrame(0xA, payload)
			continue
		case 0xA, 0x2: // pong / binary: ignore
			continue
		}
		message = append(message, payload...)
		if fin {
			return message, nil
		}
	}
}

func (ws *wsConn) writeFrame(opcode byte, payload []byte) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	header := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n < 1<<16:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		return errors.New("payload too large")
	}
	if _, err := ws.rw.Write(header); err != nil {
		return err
	}
	if _, err := ws.rw.Write(payload); err != nil {
		return err
	}
	return ws.rw.Flush()
}

func (ws *wsConn) sendEvent(event string, data interface{}) {
	msg, _ := json.Marshal(map[string]interface{}{"event": event, "data": data})
	ws.writeFrame(0x1, msg)
}

func (ws *wsConn) close() { ws.conn.Close() }

// ---------------------------------------------------------------------------
// Player session handling
// ---------------------------------------------------------------------------

var (
	playersMu         sync.Mutex
	activeUsernames   = map[string]bool{}
	controllerCounter = 0
)

type clientMessage struct {
	Event string          `json:"event"`
	Data  json.RawMessage `json:"data"`
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		return
	}
	defer ws.close()

	var username string
	var pad *gamepad
	defer func() {
		if pad != nil {
			pad.close()
		}
		if username != "" {
			playersMu.Lock()
			delete(activeUsernames, username)
			playersMu.Unlock()
			log.Printf("player %q disconnected, controller removed", username)
		}
	}()

	for {
		raw, err := ws.readMessage()
		if err != nil {
			return
		}
		var msg clientMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Event {
		case "register_player":
			var d struct {
				Username string `json:"username"`
			}
			if err := json.Unmarshal(msg.Data, &d); err != nil || d.Username == "" {
				ws.sendEvent("registration_failed", "Invalid username.")
				continue
			}
			playersMu.Lock()
			if activeUsernames[d.Username] {
				playersMu.Unlock()
				ws.sendEvent("registration_failed", fmt.Sprintf("The name %q is already taken.", d.Username))
				continue
			}
			activeUsernames[d.Username] = true
			controllerCounter++
			num := controllerCounter
			playersMu.Unlock()

			p, err := newGamepad(fmt.Sprintf("airInput Controller %d", num))
			if err != nil {
				playersMu.Lock()
				delete(activeUsernames, d.Username)
				playersMu.Unlock()
				log.Printf("failed to create gamepad for %q: %v", d.Username, err)
				ws.sendEvent("registration_failed", "Could not create virtual controller on TV.")
				continue
			}
			username = d.Username
			pad = p
			log.Printf("player %q connected as controller %d", username, num)
			ws.sendEvent("registration_success", nil)

		case "input":
			if pad == nil {
				continue
			}
			var d struct {
				Button string `json:"button"`
				State  int    `json:"state"`
			}
			if err := json.Unmarshal(msg.Data, &d); err == nil {
				pad.setButton(d.Button, d.State == 1)
			}

		case "axis":
			if pad == nil {
				continue
			}
			var d struct {
				Axis  string  `json:"axis"`
				Value float64 `json:"value"`
			}
			if err := json.Unmarshal(msg.Data, &d); err == nil {
				if _, ok := map[string]bool{"lx": true, "ly": true, "rx": true, "ry": true}[d.Axis]; ok {
					pad.setAxis(d.Axis, d.Value)
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------

func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "localhost"
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && ipnet.IP.To4() != nil {
			return ipnet.IP.String()
		}
	}
	return "localhost"
}

func main() {
	pub, err := fs.Sub(publicFiles, "public")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(http.FS(pub)))
	http.HandleFunc("/ws", handleWS)

	log.Printf("airInput TV host ready: http://%s:%d", localIP(), listenPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", listenPort), nil))
}
