// @author: kevinkelly
// @license: Apache Licence
// @file: stores.go
// @time: 2026/10/28

package archive

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dbtool/internal/archive/engine"
)

// StoreInput 是备份存储的新建 / 编辑 / 测试请求体。SK 留空 = 不修改（新建时 s3 / oss 必填）。
type StoreInput struct {
	ID            uint   `json:"id,omitempty"` // 仅 POST /stores/test：沿用已保存的 SK
	Name          string `json:"name"`
	Type          string `json:"type"`
	Path          string `json:"path"`
	Endpoint      string `json:"endpoint"`
	Region        string `json:"region"`
	Bucket        string `json:"bucket"`
	Prefix        string `json:"prefix"`
	AK            string `json:"ak"`
	SK            string `json:"sk"`
	PathStyle     bool   `json:"path_style"`
	RetentionDays int    `json:"retention_days"`
	Enabled       *bool  `json:"enabled"`
}

type lastTestView struct {
	At  string `json:"at"`
	OK  bool   `json:"ok"`
	Ms  int    `json:"ms"`
	Msg string `json:"msg"`
}

// StoreView 是备份存储的对外形态（SK 永不回显）。
type StoreView struct {
	ID            uint          `json:"id"`
	Name          string        `json:"name"`
	Type          string        `json:"type"`
	Path          string        `json:"path"`
	Endpoint      string        `json:"endpoint"`
	Region        string        `json:"region"`
	Bucket        string        `json:"bucket"`
	Prefix        string        `json:"prefix"`
	PathStyle     bool          `json:"path_style"`
	AK            string        `json:"ak"`
	SKSet         bool          `json:"sk_set"`
	RetentionDays int           `json:"retention_days"`
	Enabled       bool          `json:"enabled"`
	Status        string        `json:"status"` // ok | err | off | unknown
	LastTest      *lastTestView `json:"last_test"`
	FreeBytes     *int64        `json:"free_bytes"`
	UsedBy        int           `json:"used_by"`
	UpdatedAt     string        `json:"updated_at"`
	UpdatedBy     string        `json:"updated_by"`
}

// StorePicker 是向导里选存储的精简形态（不含地址 / 桶 / AK）。
type StorePicker struct {
	ID      uint   `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Enabled bool   `json:"enabled"`
}

func maskAK(ak string) string {
	r := []rune(ak)
	if len(r) <= 6 {
		if len(r) == 0 {
			return ""
		}
		return string(r[:1]) + "****"
	}
	return string(r[:4]) + "****" + string(r[len(r)-3:])
}

func (s *Service) storeStatus(r *StoreRow) string {
	if !r.Enabled {
		return "off"
	}
	if r.Status == "ok" || r.Status == "err" {
		return r.Status
	}
	return "unknown"
}

func (s *Service) storeView(c Caller, r *StoreRow) StoreView {
	v := StoreView{ID: r.ID, Name: r.Name, Type: r.Type, Path: r.Path, Endpoint: r.Endpoint, Region: r.Region, Bucket: r.Bucket, Prefix: r.Prefix,
		PathStyle: r.PathStyle, AK: maskAK(r.AK), SKSet: r.SKSet || r.SK != "", RetentionDays: r.RetentionDays, Enabled: r.Enabled, Status: s.storeStatus(r),
		FreeBytes: r.FreeBytes, UpdatedAt: fmtT(r.UpdatedAt), UpdatedBy: r.UpdatedBy}
	if c.has("archive.store_edit") {
		v.AK = r.AK
	}
	if r.Type != StoreLocal && r.Type != StoreNFS {
		v.FreeBytes = nil
	}
	if r.LastTestAt != nil {
		v.LastTest = &lastTestView{At: fmtT(*r.LastTestAt), OK: r.LastTestOK, Ms: r.LastTestMs, Msg: r.LastTestMsg}
	}
	if n, err := s.repo.StoreRefCount(r.ID); err == nil {
		v.UsedBy = n
	}
	return v
}

// ListStores 返回备份存储列表（archive.cfg_view）。picker=true 时返回精简列表（archive.apply / archive.dryrun 即可）。
func (s *Service) ListStores(c Caller) ([]StoreView, error) {
	if err := c.need("archive.cfg_view", "查看备份存储"); err != nil {
		return nil, err
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	rows, err := s.repo.ListStores()
	if err != nil {
		return nil, err
	}
	out := make([]StoreView, 0, len(rows))
	for i := range rows {
		out = append(out, s.storeView(c, &rows[i]))
	}
	return out, nil
}

func (s *Service) StorePickers(c Caller) ([]StorePicker, error) {
	if !c.has("archive.cfg_view") && !c.has("archive.apply") && !c.has("archive.dryrun") {
		return nil, forbidden("没有权限查看备份存储（需要 archive.cfg_view 或 archive.apply）")
	}
	if !s.repo.Ready() {
		return nil, notReady()
	}
	rows, err := s.repo.ListStores()
	if err != nil {
		return nil, err
	}
	out := make([]StorePicker, 0, len(rows))
	for i := range rows {
		out = append(out, StorePicker{ID: rows[i].ID, Name: rows[i].Name, Type: rows[i].Type, Status: s.storeStatus(&rows[i]), Enabled: rows[i].Enabled})
	}
	return out, nil
}

var absPathRe = regexp.MustCompile(`^(/|[A-Za-z]:[\\/]|\\\\)`)

func hasDotDot(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// validateStore 规整并校验；create=true 时 SK 对 s3 / oss 必填。
func (in *StoreInput) validate(create bool) []engine.Issue {
	in.Name, in.Type = strings.TrimSpace(in.Name), strings.ToLower(strings.TrimSpace(in.Type))
	in.Path, in.Endpoint, in.Region = strings.TrimSpace(in.Path), strings.TrimRight(strings.TrimSpace(in.Endpoint), "/"), strings.TrimSpace(in.Region)
	in.Bucket, in.Prefix, in.AK = strings.TrimSpace(in.Bucket), strings.Trim(strings.TrimSpace(in.Prefix), "/"), strings.TrimSpace(in.AK)
	var out []engine.Issue
	bad := func(field, format string, a ...any) {
		out = append(out, engine.Issue{Field: field, Level: "error", Msg: fmt.Sprintf(format, a...)})
	}
	if n := utf8.RuneCountInString(in.Name); n < 2 || n > 50 {
		bad("name", "名称需要 2–50 个字（当前 %d）", n)
	}
	switch in.Type {
	case StoreLocal, StoreNFS:
		if in.Path == "" {
			bad("path", "请填写目录")
		} else if !absPathRe.MatchString(in.Path) {
			bad("path", "目录需要是绝对路径")
		} else if hasDotDot(in.Path) {
			bad("path", "目录不能包含 ..")
		}
	case StoreS3, StoreOSS:
		if !strings.HasPrefix(in.Endpoint, "http://") && !strings.HasPrefix(in.Endpoint, "https://") {
			bad("endpoint", "endpoint 需要以 http:// 或 https:// 开头")
		}
		if in.Bucket == "" {
			bad("bucket", "请填写桶名")
		}
		if in.AK == "" {
			bad("ak", "请填写 AK")
		}
		if create && in.SK == "" {
			bad("sk", "请填写 SK")
		}
	default:
		bad("type", "类型只能是 local / nfs / s3 / oss")
	}
	if hasDotDot(in.Prefix) {
		bad("prefix", "路径前缀不能包含 ..")
	}
	if utf8.RuneCountInString(in.Prefix) > 200 {
		bad("prefix", "路径前缀最长 200 字")
	}
	if in.RetentionDays < 0 || in.RetentionDays > storeRetentionMax {
		bad("retention_days", "保留天数需要在 0（永久）到 %d 之间", storeRetentionMax)
	}
	return out
}

func (s *Service) needStoreEdit(c Caller) error {
	if err := c.need("archive.store_edit", "编辑备份存储"); err != nil {
		return err
	}
	if !c.IsSuperuser {
		return newErr(403, "super_only", "备份存储只有超级用户可以编辑")
	}
	if !s.repo.Ready() {
		return notReady()
	}
	return nil
}

func (s *Service) storeNameTaken(name string, except uint) (bool, error) {
	rows, err := s.repo.ListStores()
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		if r.Name == name && r.ID != except {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) CreateStore(c Caller, in StoreInput) (*StoreView, error) {
	if err := s.needStoreEdit(c); err != nil {
		return nil, err
	}
	issues := in.validate(true)
	if taken, err := s.storeNameTaken(in.Name, 0); err != nil {
		return nil, err
	} else if taken {
		issues = append(issues, engine.Issue{Field: "name", Level: "error", Msg: "存储名称已存在"})
	}
	if hasErrorIssue(issues) {
		return nil, validationErr(issues)
	}
	now := s.now()
	enabled := in.Enabled == nil || *in.Enabled
	row := &StoreRow{Name: in.Name, Type: in.Type, Path: in.Path, Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket, Prefix: in.Prefix,
		AK: in.AK, SK: in.SK, PathStyle: in.PathStyle, RetentionDays: in.RetentionDays, Enabled: enabled, Status: "unknown", UpdatedBy: c.Username, CreatedAt: now, UpdatedAt: now}
	if in.Type == StoreLocal || in.Type == StoreNFS {
		row.AK, row.SK = "", ""
	}
	if err := s.repo.InsertStore(row); err != nil {
		return nil, err
	}
	s.auditAt(c, "存储-新建", fmt.Sprintf("新建备份存储「%s」（%s）", row.Name, row.Type)) // i18n-ignore：审计 / 通知正文，不翻译
	got, err := s.repo.GetStore(row.ID)
	if err != nil || got == nil {
		return nil, err
	}
	v := s.storeView(c, got)
	return &v, nil
}

func (s *Service) UpdateStore(c Caller, id uint, in StoreInput) (*StoreView, error) {
	if err := s.needStoreEdit(c); err != nil {
		return nil, err
	}
	row, err := s.repo.GetStore(id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, notFound("备份存储不存在")
	}
	issues := in.validate(false)
	if row.Type != in.Type {
		if n, err := s.repo.StoreRefCount(id); err != nil {
			return nil, err
		} else if n > 0 {
			return nil, conflict("该存储已被 %d 个任务引用，不能修改类型", n)
		}
	}
	if (in.Type == StoreS3 || in.Type == StoreOSS) && in.SK == "" && row.SK == "" {
		issues = append(issues, engine.Issue{Field: "sk", Level: "error", Msg: "请填写 SK"})
	}
	if taken, err := s.storeNameTaken(in.Name, id); err != nil {
		return nil, err
	} else if taken {
		issues = append(issues, engine.Issue{Field: "name", Level: "error", Msg: "存储名称已存在"})
	}
	if hasErrorIssue(issues) {
		return nil, validationErr(issues)
	}
	row.Name, row.Type, row.Path, row.Endpoint, row.Region, row.Bucket, row.Prefix = in.Name, in.Type, in.Path, in.Endpoint, in.Region, in.Bucket, in.Prefix
	row.PathStyle, row.RetentionDays, row.UpdatedBy, row.UpdatedAt = in.PathStyle, in.RetentionDays, c.Username, s.now()
	if in.Enabled != nil {
		row.Enabled = *in.Enabled
	}
	keepSK := in.SK == ""
	if in.Type == StoreLocal || in.Type == StoreNFS {
		row.AK, row.SK, keepSK = "", "", false
	} else {
		row.AK = in.AK
		if !keepSK {
			row.SK = in.SK
		}
	}
	row.Status, row.LastTestAt = "unknown", nil // 配置变了，之前的测试结果不再可信
	if err := s.repo.SaveStore(row, keepSK); err != nil {
		return nil, err
	}
	s.auditAt(c, "存储-修改", fmt.Sprintf("修改备份存储「%s」（%s）%s", row.Name, row.Type, map[bool]string{true: "，更新了 SK", false: ""}[!keepSK])) // i18n-ignore：审计 / 通知正文，不翻译
	got, err := s.repo.GetStore(id)
	if err != nil || got == nil {
		return nil, err
	}
	v := s.storeView(c, got)
	return &v, nil
}

// StoreTestResult 是测试连接的结果（失败也是 200）。
type StoreTestResult struct {
	OK        bool       `json:"ok"`
	Ms        int        `json:"ms"`
	Msg       string     `json:"msg"`
	Kind      string     `json:"kind"` // ok | timeout | fail
	FreeBytes *int64     `json:"free_bytes"`
	Store     *StoreView `json:"store,omitempty"`
}

func redact(msg string, secrets ...string) string {
	for _, sct := range secrets {
		if len(sct) >= 3 {
			msg = strings.ReplaceAll(msg, sct, "***")
		}
	}
	return msg
}

func (s *Service) runStoreTest(ctx context.Context, cfg engine.StoreConfig) StoreTestResult {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	took, free, freeOK, err := s.testSt(cctx, cfg)
	res := StoreTestResult{Ms: int(took / time.Millisecond), Kind: "ok"}
	if err != nil {
		res.Kind = "fail"
		msg := redact(err.Error(), cfg.AK, cfg.SK)
		if strings.Contains(strings.ToLower(msg), "timeout") || strings.Contains(strings.ToLower(msg), "deadline exceeded") {
			res.Kind = "timeout"
		}
		res.Msg = truncRunes(fmt.Sprintf("测试失败：%s", msg), 400)
		return res
	}
	res.OK = true
	res.Msg = fmt.Sprintf("写入 / 读取 / 删除探测对象均成功（耗时 %d ms）", res.Ms)
	if freeOK {
		res.FreeBytes = &free
	}
	return res
}

// TestStore 对已保存的存储测试连接并记录结果。
func (s *Service) TestStore(ctx context.Context, c Caller, id uint) (*StoreTestResult, error) {
	if err := s.needStoreEdit(c); err != nil {
		return nil, err
	}
	row, err := s.repo.GetStore(id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, notFound("备份存储不存在")
	}
	res := s.runStoreTest(ctx, storeConfigOf(row))
	now := s.now()
	row.LastTestAt, row.LastTestOK, row.LastTestMs, row.LastTestMsg = &now, res.OK, res.Ms, res.Msg
	row.FreeBytes = res.FreeBytes
	if res.OK {
		row.Status = "ok"
	} else {
		row.Status = "err"
	}
	if err := s.repo.SaveStore(row, true); err != nil {
		return nil, err
	}
	s.auditAt(c, "存储-测试", fmt.Sprintf("测试备份存储「%s」连接：%s", row.Name, map[bool]string{true: "成功", false: "失败（" + res.Msg + "）"}[res.OK])) // i18n-ignore：审计 / 通知正文，不翻译
	if got, err := s.repo.GetStore(id); err == nil && got != nil {
		v := s.storeView(c, got)
		res.Store = &v
	}
	return &res, nil
}

// TestStoreDraft 测试未保存的表单内容；带 id 且 sk 留空时沿用已保存的 SK。不落库。
func (s *Service) TestStoreDraft(ctx context.Context, c Caller, in StoreInput) (*StoreTestResult, error) {
	if err := s.needStoreEdit(c); err != nil {
		return nil, err
	}
	if in.ID != 0 && in.SK == "" {
		if row, err := s.repo.GetStore(in.ID); err != nil {
			return nil, err
		} else if row != nil {
			in.SK = row.SK
		}
	}
	if issues := in.validate(false); hasErrorIssue(issues) {
		return nil, validationErr(issues)
	}
	cfg := engine.StoreConfig{Name: in.Name, Type: in.Type, Path: in.Path, Endpoint: in.Endpoint, Region: in.Region, Bucket: in.Bucket, Prefix: in.Prefix,
		AK: in.AK, SK: in.SK, PathStyle: in.PathStyle, RetentionDays: in.RetentionDays}
	res := s.runStoreTest(ctx, cfg)
	return &res, nil
}

func (s *Service) EnableStore(c Caller, id uint, enable bool) (*StoreView, error) {
	if err := s.needStoreEdit(c); err != nil {
		return nil, err
	}
	row, err := s.repo.GetStore(id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, notFound("备份存储不存在")
	}
	row.Enabled, row.UpdatedBy, row.UpdatedAt = enable, c.Username, s.now()
	if err := s.repo.SaveStore(row, true); err != nil {
		return nil, err
	}
	s.auditAt(c, "存储-"+map[bool]string{true: "启用", false: "停用"}[enable], fmt.Sprintf("%s备份存储「%s」", map[bool]string{true: "启用", false: "停用"}[enable], row.Name)) // i18n-ignore：审计 / 通知正文，不翻译
	v := s.storeView(c, row)
	return &v, nil
}

func (s *Service) DeleteStore(c Caller, id uint) error {
	if err := s.needStoreEdit(c); err != nil {
		return err
	}
	row, err := s.repo.GetStore(id)
	if err != nil {
		return err
	}
	if row == nil {
		return notFound("备份存储不存在")
	}
	if n, err := s.repo.StoreRefCount(id); err != nil {
		return err
	} else if n > 0 {
		return conflict("该存储被 %d 个任务引用，不能删除", n)
	}
	if err := s.repo.DeleteStore(id); err != nil {
		return err
	}
	s.auditAt(c, "存储-删除", fmt.Sprintf("删除备份存储「%s」", row.Name)) // i18n-ignore：审计 / 通知正文，不翻译
	return nil
}
