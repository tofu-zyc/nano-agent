package channel

import "context"

// 扫码绑定机器人（QQ、微信）：网关向平台要一个二维码，用户用手机扫码确认后，网关拿到机器人的凭据。
// 飞书的扫码注册见 feishu_register.go（它直接写进飞书节点的配置，流程不一样）。

// BotCredentials 是扫码拿到的机器人凭据。
type BotCredentials struct {
	AppID     string // QQ
	AppSecret string // QQ
	Token     string // 微信的 bot_token
	BaseURL   string // 微信：之后调接口用的地址
	BotID     string // 微信的 ilink_bot_id
	Owner     string // 扫码的人在这个机器人那里的 ID（网关把他设成管理员）
}

// 扫码过程中的进展
const (
	RegScanned     = "scanned"      // 扫了，等手机上确认
	RegVerify      = "verify"       // 要输入手机上显示的数字（Verify）
	RegVerifyWrong = "verify_wrong" // 输的数字不对，再输一次
	RegRefreshed   = "refreshed"    // 二维码过期了，换了一个新的（url 是新的二维码内容）
)

// Registration 是一次进行中的扫码绑定。
type Registration interface {
	// QRURL 是二维码的内容
	QRURL() string
	// Wait 等扫码结果。progress 报告进展（RegScanned……），刷新二维码时 url 是新的内容。
	Wait(ctx context.Context, progress func(stage, url string)) (*BotCredentials, error)
	// Verify 交上手机上显示的数字（只有微信会要）
	Verify(code string)
}
