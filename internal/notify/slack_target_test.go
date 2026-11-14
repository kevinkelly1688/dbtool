// @author: kevinkelly
// @license: Apache Licence
// @file: slack_target_test.go
// @time: 2026/10/28

package notify

import "testing"

// Slack 里复制出来的各种 @ 写法规整成系统使用的标识。
func TestNormalizeSlackTarget(t *testing.T) {
	for in, want := range map[string]string{
		"S0ARY4FQATB":                   "S0ARY4FQATB",
		"!subteam^S0ARY4FQATB":          "S0ARY4FQATB",
		"<!subteam^S0ARY4FQATB>":        "S0ARY4FQATB",
		"  <!subteam^S0ARY4FQATB|@dba>": "S0ARY4FQATB",
		"subteam^S0ARY4FQATB":           "S0ARY4FQATB",
		"U012AB3CD":                     "U012AB3CD",
		"<@U012AB3CD>":                  "U012AB3CD",
		"@U012AB3CD":                    "U012AB3CD",
		"<@U012AB3CD|alice>":            "U012AB3CD",
		"here":                          "here",
		"<!here>":                       "here",
		"!channel":                      "channel",
		"@everyone":                     "everyone",
		"@Here":                         "here",
		"a b<c>":                        "a b<c>", // 不是 Slack 的写法：原样返回，由格式校验拒绝
		"":                              "",
	} {
		if got := NormalizeSlackTarget(in); got != want {
			t.Errorf("NormalizeSlackTarget(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 即使存的是 Slack 的原写法，发送时也能 @ 到用户组 / 用户；@ 前缀里不会带出未识别的内容。
func TestSlackMentionsAcceptsPastedMarkup(t *testing.T) {
	got := slackMentions([]Mention{{Raw: "!subteam^S0ARY4FQATB"}, {Raw: "<@U012AB3CD>"}, {Raw: "<!here>"}, {Raw: "bad value"}})
	want := "<!subteam^S0ARY4FQATB> <@U012AB3CD> <!here> "
	if got != want {
		t.Errorf("slackMentions = %q，期望 %q", got, want)
	}
}
