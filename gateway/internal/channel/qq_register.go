package channel

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"nano-agent/gateway/internal/httpx"
)

// 扫码创建 QQ 机器人：QQ 开放平台给 agent 用的绑定接口，和腾讯的 @tencent-connect/qqbot-connector 是同一套。
//  1. create_bind_task：带上自己生成的 32 字节密钥，拿到任务 ID；
//  2. 用手机 QQ 扫 connect.html?task_id=… 的二维码，在手机上创建（或选一个已有的）机器人；
//  3. poll_bind_result：拿到 AppID 和用密钥加密过的 AppSecret（AES-256-GCM），还有扫码人的 openid。
//
// 二维码过期了就换一个新任务（最多换 3 次）。

// QQConnectBase 是绑定接口的地址（测试时换成假服务）
var QQConnectBase = "https://q.qq.com"

const qqMaxRefresh = 3

type qqRegistration struct {
	hc       *http.Client
	base     string
	task     string
	key      string
	url      string
	interval time.Duration
}

// BeginQQRegistration 开始一次扫码。
func BeginQQRegistration(ctx context.Context, proxy string) (Registration, error) {
	hc, err := httpx.NewClient(proxy, 20*time.Second)
	if err != nil {
		return nil, err
	}
	r := &qqRegistration{hc: hc, base: QQConnectBase, interval: 2 * time.Second}
	if err := r.newTask(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *qqRegistration) QRURL() string { return r.url }
func (r *qqRegistration) Verify(string) {}

func (r *qqRegistration) newTask(ctx context.Context) error {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return err
	}
	key := base64.StdEncoding.EncodeToString(k)
	var resp struct {
		Retcode int    `json:"retcode"`
		Msg     string `json:"msg"`
		Data    struct {
			TaskID string `json:"task_id"`
		} `json:"data"`
	}
	if err := r.call(ctx, "/lite/create_bind_task", map[string]string{"key": key}, &resp); err != nil {
		return fmt.Errorf("向 QQ 要二维码：%w", err)
	}
	if resp.Retcode != 0 || resp.Data.TaskID == "" {
		return fmt.Errorf("QQ 没有给二维码：%d %s", resp.Retcode, resp.Msg)
	}
	r.task, r.key = resp.Data.TaskID, key
	r.url = "https://q.qq.com/qqbot/openclaw/connect.html?task_id=" + url.QueryEscape(r.task) + "&source=&_wv=2"
	return nil
}

func (r *qqRegistration) Wait(ctx context.Context, progress func(stage, url string)) (*BotCredentials, error) {
	refreshed := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.interval):
		}
		var resp struct {
			Retcode int    `json:"retcode"`
			Msg     string `json:"msg"`
			Data    struct {
				Status   int             `json:"status"`    // 1 等扫码 2 好了 3 过期
				AppID    json.RawMessage `json:"bot_appid"` // 可能是数字也可能是字符串
				Secret   string          `json:"bot_encrypt_secret"`
				UserOpen string          `json:"user_openid"`
			} `json:"data"`
		}
		if err := r.call(ctx, "/lite/poll_bind_result", map[string]string{"task_id": r.task}, &resp); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue // 网络抖一下：接着问
		}
		if resp.Retcode != 0 {
			return nil, fmt.Errorf("QQ 返回错误：%d %s", resp.Retcode, resp.Msg)
		}
		switch resp.Data.Status {
		case 2:
			secret, err := qqDecrypt(resp.Data.Secret, r.key)
			if err != nil {
				return nil, fmt.Errorf("解开 AppSecret：%w", err)
			}
			appID := strings.Trim(string(resp.Data.AppID), `"`)
			if appID == "" || appID == "null" || appID == "0" || secret == "" {
				return nil, errors.New("QQ 没有返回机器人的凭据")
			}
			return &BotCredentials{AppID: appID, AppSecret: secret, Owner: resp.Data.UserOpen}, nil
		case 3:
			if refreshed >= qqMaxRefresh {
				return nil, errors.New("二维码过期了，请重新生成")
			}
			refreshed++
			if err := r.newTask(ctx); err != nil {
				return nil, err
			}
			progress(RegRefreshed, r.url)
		}
	}
}

func (r *qqRegistration) call(ctx context.Context, path string, in, out any) error {
	b, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// qqDecrypt 解开 AES-256-GCM 加密的 AppSecret：密文是 base64 的 IV（12 字节）+ 密文 + 校验（16 字节）。
func qqDecrypt(enc, key string) (string, error) {
	k, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	if len(data) < 12+16 {
		return "", errors.New("密文太短")
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, data[:12], data[12:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
