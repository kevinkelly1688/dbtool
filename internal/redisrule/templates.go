// @author: kevinkelly
// @license: Apache Licence
// @file: templates.go
// @time: 2026/10/28

package redisrule

// Template 是“从模板添加”里的常见规则。点选后只是把字段填进编辑器，仍需管理员确认保存。
type Template struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Rule        Rule   `json:"rule"`
}

// Templates 返回内置模板（默认不启用任何规则：库里预置 0 条）。
func Templates() []Template {
	return []Template{
		{Key: "clean-cache", Title: "清理缓存键", Description: "允许 DEL / UNLINK 删除 cache: 开头的键，一次最多 100 个。",
			Rule: Rule{Name: "清理缓存键", Enabled: true, Commands: []string{"DEL", "UNLINK"}, KeyPatterns: []string{"cache:*"}, MaxKeys: 100,
				Description: "清理缓存键：DEL / UNLINK cache:*"}},
		{Key: "set-expire", Title: "设置过期", Description: "允许对 session: 开头的键设置 / 修改过期时间。",
			Rule: Rule{Name: "设置会话键过期", Enabled: true, Commands: []string{"EXPIRE", "PEXPIRE", "EXPIREAT", "PEXPIREAT"}, KeyPatterns: []string{"session:*"}, MaxKeys: 1,
				Description: "设置过期：EXPIRE / PEXPIRE session:*"}},
		{Key: "write-cache", Title: "读后写入（写缓存键）", Description: "允许 SET / SETEX / PSETEX 写 cache: 开头的键（建议带过期时间；SET 不带过期会给警告）。",
			Rule: Rule{Name: "写入缓存键", Enabled: true, Commands: []string{"SET", "SETEX", "PSETEX"}, KeyPatterns: []string{"cache:*"}, MaxKeys: 1,
				Description: "写入缓存：SET / SETEX cache:*", RecordBefore: false}},
		{Key: "fix-hash", Title: "修复哈希字段", Description: "允许对 user: 开头的哈希修改 / 删除字段，并记录执行前的字段值供恢复。",
			Rule: Rule{Name: "修复用户哈希字段", Enabled: true, Commands: []string{"HSET", "HDEL"}, KeyPatterns: []string{"user:*"}, MaxKeys: 1,
				Description: "修复哈希字段：HSET / HDEL user:*", RecordBefore: true}},
	}
}
