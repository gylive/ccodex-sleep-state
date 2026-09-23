package gateway

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gylive/ccodex-sleep-state/internal/settings"
)

// accountPolicy is a local collection heuristic, never proof of a subscription
// or of model quality. The upstream remains responsible for authentication.
type accountPolicy struct {
	Mode            string `json:"account_mode"`
	Source          string `json:"account_source"`
	Blocks          int    `json:"expected_blocks"`
	Length          int    `json:"expected_length"`
	AcceptedBlocks  []int  `json:"accepted_blocks"`
	AcceptedLengths []int  `json:"accepted_lengths"`
	Note            string `json:"account_note"`
}

func policyFor(c settings.Config, h http.Header) accountPolicy {
	p := accountPolicy{Mode: "personal", Source: "default", Blocks: 10, Note: "未识别到套餐提示，暂用个人规则；Team 用户可手动选择。长度只是经验规则，不代表模型质量。"}
	switch c.AccountMode {
	case "personal", "team":
		p.Mode, p.Source, p.Note = c.AccountMode, "manual", "使用手动选择的账号规则；长度只是经验规则，不代表模型质量。"
	default:
		if c.BaselineBlocks != 10 && c.BaselineBlocks > 0 {
			p.Mode, p.Source, p.Blocks, p.Note = "custom", "legacy_baseline", c.BaselineBlocks, "沿用旧配置 baseline_blocks；可切换为个人或 Team 规则。"
		} else if mode := planHint(h); mode != "" {
			p.Mode, p.Source, p.Note = mode, "token_hint", "按登录令牌中的套餐提示选择规则，未验证该提示；长度不代表模型质量。"
		}
	}
	if p.Mode == "team" {
		p.Blocks = 12
	}
	p.Length = base64.URLEncoding.EncodedLen(57 + 16*p.Blocks)
	p.AcceptedBlocks = []int{p.Blocks}
	// Issue #13 reports a 33-block envelope for personal and Team accounts.
	// Keep explicit legacy custom baselines exact; do not learn rules from responses.
	if p.Mode != "custom" {
		p.AcceptedBlocks = append(p.AcceptedBlocks, 33)
	}
	for _, blocks := range p.AcceptedBlocks {
		p.AcceptedLengths = append(p.AcceptedLengths, base64.URLEncoding.EncodedLen(57+16*blocks))
	}
	return p
}

func (p accountPolicy) shapeDescription() string {
	shapes := make([]string, 0, len(p.AcceptedBlocks))
	for i, blocks := range p.AcceptedBlocks {
		shapes = append(shapes, fmt.Sprintf("%d 块（约 %d 字符）", blocks, p.AcceptedLengths[i]))
	}
	return strings.Join(shapes, " 或 ")
}

func planHint(h http.Header) string {
	auth := strings.TrimSpace(strings.TrimPrefix(h.Get("Authorization"), "Bearer "))
	parts := strings.Split(auth, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) > 12288 {
		return ""
	}
	var claims struct {
		Auth struct {
			Plan    string `json:"chatgpt_plan_type"`
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	// A selected workspace may differ from the token's default workspace. Do
	// not apply a personal plan hint to a different selected Team account.
	if selected := h.Get("ChatGPT-Account-Id"); selected != "" && selected != claims.Auth.Account {
		return ""
	}
	switch strings.ToLower(claims.Auth.Plan) {
	case "team", "business":
		return "team"
	case "free", "plus", "pro":
		return "personal"
	default:
		return ""
	}
}
