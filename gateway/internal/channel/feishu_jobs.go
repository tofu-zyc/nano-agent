package channel

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func feishuJobCard(c JobCard) string {
	// Code formatting avoids interpreting command fragments as card markup.
	els := []any{markdown("", "```\n"+strings.ReplaceAll(streamClip(JobCardText(c), 3800), "```", "ˋˋˋ")+"\n```")}
	columns := []any{}
	for _, b := range c.Buttons {
		if len(columns) == 3 {
			break
		}
		if validJobButton(b.Token) {
			columns = append(columns, map[string]any{"tag": "column", "width": "auto", "elements": []any{
				fsButton("", streamClip(b.Label, 30), "default", map[string]string{fsAction: ActionJob, "token": b.Token}),
			}})
		}
	}
	if len(columns) > 0 {
		els = append(els, map[string]any{"tag": "column_set", "flex_mode": "none", "columns": columns})
	}
	raw, _ := json.Marshal(map[string]any{"schema": "2.0", "config": map[string]any{"update_multi": true,
		"summary": map[string]string{"content": "执行任务 · " + c.Status}}, "body": map[string]any{"elements": els}})
	return string(raw)
}
func (f *Feishu) SendJob(ctx context.Context, chat string, c JobCard) (string, error) {
	return f.sendMsg(ctx, chat, "", "interactive", feishuJobCard(c))
}
func (f *Feishu) UpdateJob(ctx context.Context, chat, ref string, c JobCard) error {
	// State refresh uses the ordinary message projection API. The callback
	// returns only an acknowledgement; it never claims execution succeeded.
	out, err := f.client.Im.Message.Patch(ctx, larkim.NewPatchMessageReqBuilder().MessageId(ref).
		Body(larkim.NewPatchMessageReqBodyBuilder().Content(feishuJobCard(c)).Build()).Build())
	if err != nil {
		return err
	}
	if !out.Success() {
		return fmt.Errorf("更新任务卡片失败：%d %s", out.Code, out.Msg)
	}
	return nil
}
