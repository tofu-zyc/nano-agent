package kernel

// 配对：别的电脑上的内核不用知道网关的口令。启动包里只有网关地址，内核第一次连上来时没有口令，
// 请求配对；网关登记一个待确认的请求，网页上显示电脑的名字、地址和确认码（内核那边也打印同一个
// 确认码），管理员核对后点允许，网关给这台电脑生成一个它专用的口令，内核存下来重新连接。
//
// 配对得到的口令只能用这台电脑配对时的名字，冒充不了别的节点；网页里移除这台电脑，口令就作废。
// 网关只存口令的 SHA-256。

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"nano-agent/gateway/internal/proto"
)

const (
	pairTimeout = 10 * time.Minute // 等管理员确认的时间
	maxPending  = 16
)

// PairRequest 是一个等管理员确认的配对请求。
type PairRequest struct {
	ID   string    `json:"id"`
	Node string    `json:"node"`
	Addr string    `json:"addr"` // 内核连过来的地址
	Code string    `json:"code"` // 确认码，内核那边也会打印
	At   time.Time `json:"at"`

	decide chan bool
}

// PairedDevice 是配对过的一台电脑。
type PairedDevice struct {
	Node  string    `json:"node"`
	Addr  string    `json:"addr"` // 配对时的地址
	Since time.Time `json:"since"`
	Hash  string    `json:"-"` // 口令的 SHA-256
}

type pairedFile struct {
	Node  string    `json:"node"`
	Addr  string    `json:"addr"`
	Since time.Time `json:"since"`
	Hash  string    `json:"hash"`
}

// EnablePairing 打开配对，配对过的电脑存在 path（JSON）。不调用的话只认共用口令。
func (h *Hub) EnablePairing(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.pairPath = path
	h.paired = map[string]PairedDevice{}
	h.pending = map[string]*PairRequest{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []pairedFile
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("读取配对记录 %s：%w", path, err)
	}
	for _, d := range list {
		h.paired[d.Node] = PairedDevice{Node: d.Node, Addr: d.Addr, Since: d.Since, Hash: d.Hash}
	}
	return nil
}

// OnPairChange 在配对请求和配对过的电脑有变化时调用（网页刷新）。
func (h *Hub) OnPairChange(f func()) {
	h.mu.Lock()
	h.pairNotify = f
	h.mu.Unlock()
}

func (h *Hub) pairChanged() {
	h.mu.Lock()
	f := h.pairNotify
	h.mu.Unlock()
	if f != nil {
		f()
	}
}

// PairRequests 返回等待确认的配对请求，先来的在前。
func (h *Hub) PairRequests() []PairRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []PairRequest{}
	for _, r := range h.pending {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// PairedDevices 返回配对过的电脑，按名字排序。
func (h *Hub) PairedDevices() []PairedDevice {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []PairedDevice{}
	for _, d := range h.paired {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// DecidePair 允许或拒绝一个配对请求。
func (h *Hub) DecidePair(id string, ok bool) error {
	h.mu.Lock()
	r := h.pending[id]
	h.mu.Unlock()
	if r == nil {
		return errors.New("这个配对请求已经不在了（电脑断开了，或者已经处理过）")
	}
	select {
	case r.decide <- ok:
	default:
	}
	return nil
}

// Unpair 移除一台配对过的电脑：它的口令作废，在线的话断开。
func (h *Hub) Unpair(node string) error {
	h.mu.Lock()
	if _, ok := h.paired[node]; !ok {
		h.mu.Unlock()
		return errors.New("没有配对过这台电脑")
	}
	delete(h.paired, node)
	err := h.savePaired()
	c := h.conns[node]
	h.mu.Unlock()
	if c != nil && c.paired {
		c.close()
	}
	h.pairChanged()
	return err
}

// savePaired 把配对记录写到磁盘。调用方持有 h.mu。
func (h *Hub) savePaired() error {
	list := []pairedFile{}
	for _, d := range h.paired {
		list = append(list, pairedFile{d.Node, d.Addr, d.Since, d.Hash})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Node < list[j].Node })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := h.pairPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.pairPath)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// pairedToken 判断 token 是不是 node 配对时拿到的口令。
func (h *Hub) pairedToken(node, token string) bool {
	h.mu.Lock()
	d, ok := h.paired[node]
	h.mu.Unlock()
	return ok && subtle.ConstantTimeCompare([]byte(hashToken(token)), []byte(d.Hash)) == 1
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// pair 处理一个请求配对的连接：登记请求，等管理员确认，同意的话发口令，然后断开（内核拿口令重连）。
func (h *Hub) pair(nc net.Conn, sc *bufio.Scanner, hello *proto.Msg) {
	var wmu sync.Mutex
	write := func(m proto.Msg) {
		b, _ := json.Marshal(m)
		wmu.Lock()
		defer wmu.Unlock()
		nc.SetWriteDeadline(time.Now().Add(writeTimeout))
		nc.Write(append(b, '\n'))
	}
	node := hello.Node
	host := "本机"
	if a, ok := nc.RemoteAddr().(*net.TCPAddr); ok {
		host = a.IP.String()
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	r := &PairRequest{ID: randomHex(8), Node: node, Addr: host, Code: fmt.Sprintf("%06d", n.Int64()), At: time.Now(), decide: make(chan bool, 1)}

	h.mu.Lock()
	why := ""
	switch {
	case h.pairPath == "":
		why = "这个网关没有打开配对，请在 kernel.conf 里填 token"
	case h.paired[node].Hash != "":
		why = fmt.Sprintf("名字 %s 已经有电脑配对过了。换个名字，或者先在网关网页的拓扑图上点那台电脑，移除旧的", node)
	case h.conns[node] != nil:
		why = fmt.Sprintf("已经有叫 %s 的内核在线，换个名字", node)
	case len(h.pending) >= maxPending:
		why = "等待确认的配对请求太多了，稍后再试"
	}
	for _, p := range h.pending {
		if p.Node == node {
			why = fmt.Sprintf("叫 %s 的电脑已经在等待确认，换个名字", node)
		}
	}
	if why == "" {
		h.pending[r.ID] = r
	}
	h.mu.Unlock()
	if why != "" {
		h.log.Warn("拒绝配对请求", "node", node, "addr", host, "why", why)
		write(proto.Msg{T: proto.TError, Detail: why})
		return
	}
	h.log.Info("收到配对请求，等管理员在网页上确认", "node", node, "addr", host, "code", r.Code)
	write(proto.Msg{T: proto.TPairing, Code: r.Code})
	h.pairChanged()

	// 等待期间内核照常 ping，回 pong；连接断了就撤销请求
	gone := make(chan struct{})
	var finishing atomic.Bool
	go func() {
		defer close(gone)
		for {
			if !finishing.Load() {
				nc.SetReadDeadline(time.Now().Add(readTimeout))
			}
			m, err := readMsg(sc)
			if err != nil {
				return
			}
			if m.T == proto.TPing {
				write(proto.Msg{T: proto.TPong})
			}
		}
	}()
	timer := time.NewTimer(pairTimeout)
	defer timer.Stop()
	decided, ok := false, false
	select {
	case ok = <-r.decide:
		decided = true
	case <-gone:
	case <-timer.C:
	}

	h.mu.Lock()
	delete(h.pending, r.ID)
	var token string
	var saveErr error
	if ok {
		token = randomHex(32)
		h.paired[node] = PairedDevice{Node: node, Addr: host, Since: time.Now(), Hash: hashToken(token)}
		if saveErr = h.savePaired(); saveErr != nil {
			delete(h.paired, node)
		}
	}
	h.mu.Unlock()
	h.pairChanged()

	switch {
	case !decided:
		h.log.Info("配对请求没等到确认", "node", node)
		write(proto.Msg{T: proto.TError, Detail: "等了太久没人确认，过一会儿会重新申请"})
	case saveErr != nil:
		h.log.Error("保存配对记录失败", "err", saveErr)
		write(proto.Msg{T: proto.TError, Detail: "网关保存配对记录失败：" + saveErr.Error()})
	case ok:
		h.log.Info("配对成功", "node", node, "addr", host)
		write(proto.Msg{T: proto.TPaired, Token: token})
	default:
		h.log.Info("管理员拒绝了配对", "node", node, "addr", host)
		write(proto.Msg{T: proto.TError, Reason: proto.ErrDenied, Detail: "网关管理员拒绝了这台电脑接入"})
	}
	// 等内核读完再断开（它收到后会自己断开）。直接关的话，没读的 ping 可能让对方收到 RST，丢掉最后这条
	finishing.Store(true)
	if cw, ok := nc.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	<-gone
}
