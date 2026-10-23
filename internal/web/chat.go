package web

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LECYWZA/mytun/internal/mux"
)

// ChatMessage is one text message exchanged over the tunnel.
type ChatMessage struct {
	ID   string `json:"id"`
	TS   int64  `json:"ts"`
	From string `json:"from"`
	To   string `json:"to"`
	Text string `json:"text"`
	Dir  string `json:"dir"`
	Via  string `json:"via"`
}

// FileRecord is one transferred file.
type FileRecord struct {
	ID      string `json:"id"`
	TS      int64  `json:"ts"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Dir     string `json:"dir"`
	To      string `json:"to,omitempty"`
	SavedAs string `json:"savedAs,omitempty"`
	From    string `json:"from"`
	Via     string `json:"via"`
}

// Chat implements message and file transfer over tunnel channels. The wire
// format is compatible with Bi-Tunnel: meta{type:"msg"} with the text as body,
// and meta{type:"file",name,size} with the raw stream as body.
type Chat struct {
	mu       sync.Mutex
	messages []ChatMessage
	files    []FileRecord
	recvDir  string
	local    string
	maxMsgs  int
	onEvent  func(string, any)
}

// NewChat creates the chat service storing received files in dir.
func NewChat(dir, label string, onEvent func(string, any)) *Chat {
	_ = os.MkdirAll(dir, 0o755)
	return &Chat{recvDir: dir, local: label, maxMsgs: 500, onEvent: onEvent}
}

func (c *Chat) emit(ev string, v any) {
	if c.onEvent != nil {
		c.onEvent(ev, v)
	}
}

// HandleChannel dispatches inbound msg/file channels; returns handled.
func (c *Chat) HandleChannel(sess *mux.Session, ch *mux.Channel) bool {
	typ, _ := ch.Meta["type"].(string)
	switch typ {
	case mux.MetaMsg:
		go c.recvMsg(sess, ch)
		return true
	case mux.MetaFile:
		go c.recvFile(sess, ch)
		return true
	}
	return false
}

func (c *Chat) recvMsg(sess *mux.Session, ch *mux.Channel) {
	data, err := io.ReadAll(io.LimitReader(ch, 8<<20))
	if err != nil {
		return
	}
	metaStr := func(k string) string { s, _ := ch.Meta[k].(string); return s }
	msg := ChatMessage{
		ID:   firstNonEmpty(metaStr("id"), strconv.FormatInt(time.Now().UnixNano(), 36)),
		TS:   metaInt64(ch.Meta["ts"]),
		From: firstNonEmpty(metaStr("sender"), sess.ClientID, "remote"),
		To:   metaStr("target"),
		Text: string(data),
		Dir:  "in",
		Via:  sess.Role(),
	}
	if msg.TS == 0 {
		msg.TS = time.Now().UnixMilli()
	}
	c.mu.Lock()
	c.messages = append(c.messages, msg)
	if len(c.messages) > c.maxMsgs {
		c.messages = c.messages[len(c.messages)-c.maxMsgs:]
	}
	c.mu.Unlock()
	c.emit("chat_message", msg)
	_ = ch.Close()
}

func (c *Chat) recvFile(sess *mux.Session, ch *mux.Channel) {
	name, _ := ch.Meta["name"].(string)
	if name == "" {
		name = "file.bin"
	}
	safe := filepath.Base(name)
	tmp := filepath.Join(c.recvDir, "."+strconv.FormatInt(time.Now().UnixNano(), 36)+".part")
	f, err := os.Create(tmp)
	if err != nil {
		_ = ch.Close()
		return
	}
	n, _ := io.Copy(f, ch)
	_ = f.Close()
	_ = ch.Close()

	final := filepath.Join(c.recvDir, safe)
	if _, err := os.Stat(final); err == nil {
		ext := filepath.Ext(safe)
		base := strings.TrimSuffix(safe, ext)
		final = filepath.Join(c.recvDir, base+"_"+strconv.FormatInt(time.Now().Unix(), 10)+ext)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return
	}
	rec := FileRecord{
		ID:      firstNonEmpty(metaStr(ch.Meta, "id"), strconv.FormatInt(time.Now().UnixNano(), 36)),
		TS:      time.Now().UnixMilli(),
		Name:    safe,
		Size:    n,
		Dir:     "in",
		SavedAs: filepath.Base(final),
		From:    metaStr(ch.Meta, "sender"),
		Via:     sess.Role(),
	}
	c.mu.Lock()
	c.files = append(c.files, rec)
	c.mu.Unlock()
	c.emit("chat_file", rec)
}

// SendText sends a one-shot message channel.
func (c *Chat) SendText(sess *mux.Session, text, target string) (ChatMessage, error) {
	meta := map[string]any{
		"type":   mux.MetaMsg,
		"id":     strconv.FormatInt(time.Now().UnixNano(), 36),
		"ts":     time.Now().UnixMilli(),
		"sender": "console",
		"target": target,
	}
	ch, err := sess.OpenChannel(meta)
	if err != nil {
		return ChatMessage{}, err
	}
	if _, err := ch.Write([]byte(text)); err != nil {
		_ = ch.Close()
		return ChatMessage{}, err
	}
	_ = sess.Flush()
	_ = ch.Close()
	msg := ChatMessage{ID: meta["id"].(string), TS: meta["ts"].(int64), From: c.local, To: target,
		Text: text, Dir: "out", Via: sess.Role()}
	c.mu.Lock()
	c.messages = append(c.messages, msg)
	if len(c.messages) > c.maxMsgs {
		c.messages = c.messages[len(c.messages)-c.maxMsgs:]
	}
	c.mu.Unlock()
	c.emit("chat_message", msg)
	return msg, nil
}

// SendFile streams a local file over a tunnel channel. displayName overrides the
// name announced to the peer (used when the local path is a temporary upload).
func (c *Chat) SendFile(sess *mux.Session, path, target string, displayName ...string) (FileRecord, error) {
	st, err := os.Stat(path)
	if err != nil {
		return FileRecord{}, err
	}
	name := filepath.Base(path)
	if len(displayName) > 0 && displayName[0] != "" {
		name = filepath.Base(displayName[0])
	}
	meta := map[string]any{
		"type":   mux.MetaFile,
		"id":     strconv.FormatInt(time.Now().UnixNano(), 36),
		"ts":     time.Now().UnixMilli(),
		"name":   name,
		"size":   st.Size(),
		"sender": "console",
		"target": target,
	}
	ch, err := sess.OpenChannel(meta)
	if err != nil {
		return FileRecord{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		_ = ch.Close()
		return FileRecord{}, err
	}
	defer f.Close()
	if _, err := io.Copy(ch, f); err != nil {
		_ = ch.Close()
		return FileRecord{}, err
	}
	_ = sess.Flush()
	_ = ch.Close()
	rec := FileRecord{ID: meta["id"].(string), TS: meta["ts"].(int64), Name: name, Size: st.Size(),
		Dir: "out", To: target, Via: sess.Role()}
	c.mu.Lock()
	c.files = append(c.files, rec)
	c.mu.Unlock()
	c.emit("chat_file", rec)
	return rec, nil
}

// Messages returns the in-memory history.
func (c *Chat) Messages() []ChatMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ChatMessage, len(c.messages))
	copy(out, c.messages)
	return out
}

// Files returns the transfer records.
func (c *Chat) Files() []FileRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]FileRecord, len(c.files))
	copy(out, c.files)
	return out
}

// ListFiles describes files actually present in the receive directory.
func (c *Chat) ListFiles() []map[string]any {
	entries, err := os.ReadDir(c.recvDir)
	if err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, map[string]any{"name": e.Name(), "size": info.Size(), "mtime": info.ModTime().UnixMilli()})
	}
	return out
}

// ReceiveDir exposes the download directory.
func (c *Chat) ReceiveDir() string { return c.recvDir }

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func metaStr(m map[string]any, k string) string {
	if m == nil {
		return ""
	}
	s, _ := m[k].(string)
	return s
}

func metaInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func listenCheck(port int) (net.Listener, error) {
	return net.Listen("tcp", "0.0.0.0:"+strconv.Itoa(port))
}
