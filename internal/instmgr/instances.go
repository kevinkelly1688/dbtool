// @author: kevinkelly
// @license: Apache Licence
// @file: instances.go
// @time: 2026/10/28

package instmgr

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"dbtool/internal/audit"
	"dbtool/internal/i18n"
	"dbtool/internal/instext"
	"dbtool/internal/model"
)

// ---- 实例列表 ----

// ListQuery 是实例列表的筛选与分页。
type ListQuery struct {
	Role     string // master / slave
	DBType   string
	Tag      string // 标签代码
	Status   string // "1" 慢日志采集已开启 / "0" 未开启 / 空 全部
	State    string // disabled 已停用 / enabled 未停用 / 空 全部（旁路表 dbt_instance_ext）
	Env      string // prod / test
	Q        string
	Page     int
	PageSize int
}

// InstanceItem 是列表里的一行，也是编辑表单的初值。不含密码（连是否已设置都不暴露给无关字段）。
type InstanceItem struct {
	ID          uint          `json:"id"`
	Name        string        `json:"instance_name"`
	Role        string        `json:"type"`
	DBType      string        `json:"db_type"`
	DBTypeLabel string        `json:"db_type_label"`
	Mode        string        `json:"mode"`
	Host        string        `json:"host"`
	Port        int           `json:"port"`
	User        string        `json:"user"`
	IsSSL       bool          `json:"is_ssl"`
	VerifySSL   bool          `json:"verify_ssl"`
	IsEnable    bool          `json:"is_enable"`
	DBName      string        `json:"db_name"`
	ShowRegex   string        `json:"show_db_name_regex"`
	DeniedRegex string        `json:"denied_db_name_regex"`
	Charset     string        `json:"charset"`
	ServiceName string        `json:"service_name"`
	SID         string        `json:"sid"`
	TunnelID    uint          `json:"tunnel_id"`
	TunnelName  string        `json:"tunnel_name"`
	Groups      []GroupOption `json:"groups"`
	Tags        []TagOption   `json:"tags"`
	CanBinlog   bool          `json:"can_binlog"`  // 支持 Binlog 清理（仅 mysql）
	Disabled    bool          `json:"disabled"`    // 实例已停用（旁路表）
	DisabledBy  string        `json:"disabled_by"` // 停用人
	DisabledAt  string        `json:"disabled_at"`
	Env         string        `json:"env"` // 环境字典里的编码 / 空
	EnvLabel    string        `json:"env_label"`
	EnvProd     bool          `json:"env_prod"` // 按生产对待（红色徽标）
	// 仅 PostgreSQL / MongoDB / ClickHouse：支持的功能（query）及其中文说明；其它功能的页面对这些类型提示“暂不支持”。
	Features      []string `json:"features,omitempty"`
	FeaturesLabel string   `json:"features_label,omitempty"`
}

// InstanceList 是一页实例。
type InstanceList struct {
	Total int64          `json:"total"`
	Items []InstanceItem `json:"items"`
	// Stats 是可见范围内（不受当前筛选影响）的实例数；旁路表不可用时为 nil。
	Stats    *InstanceStats `json:"stats,omitempty"`
	ExtReady bool           `json:"ext_ready"` // 旁路表 dbt_instance_ext 是否已建（停用 / 环境功能可用）
}

func (s *Service) toItem(inst *model.Instance) InstanceItem {
	it := InstanceItem{ID: inst.ID, Name: inst.InstanceName, Role: inst.Type, DBType: inst.DBType, DBTypeLabel: dbTypeLabel(inst.DBType),
		Mode: inst.Mode, Host: inst.Host, Port: inst.Port, User: s.repo.Decrypt(inst.User), IsSSL: inst.IsSSL, VerifySSL: inst.VerifySSL,
		IsEnable: inst.IsEnable, DBName: inst.DBName, ShowRegex: inst.ShowDBNameRegex, DeniedRegex: inst.DeniedDBNameRegex,
		Charset: inst.Charset, Groups: []GroupOption{}, Tags: []TagOption{}, CanBinlog: supportsBinlogPurge(inst.DBType)}
	if inst.ServiceName != nil {
		it.ServiceName = *inst.ServiceName
	}
	if inst.SID != nil {
		it.SID = *inst.SID
	}
	if inst.TunnelID != nil {
		it.TunnelID = *inst.TunnelID
		if inst.Tunnel != nil {
			it.TunnelName = inst.Tunnel.TunnelName
		}
	}
	for _, g := range inst.ResourceGroups {
		it.Groups = append(it.Groups, GroupOption{ID: g.GroupID, Name: g.GroupName})
	}
	for _, t := range inst.Tags {
		it.Tags = append(it.Tags, TagOption{ID: t.ID, Code: t.TagCode, Name: t.TagName, Active: t.Active})
	}
	it.Features, it.FeaturesLabel = featuresOf(inst.DBType)
	return it
}

// scopeOf 返回调用者的实例可见范围：nil 表示全部。
func (s *Service) scopeOf(c Caller) (map[uint]bool, error) {
	if c.AllInstances {
		return nil, nil
	}
	ids, err := s.repo.UserInstanceIDs(c.ID)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = map[uint]bool{}
	}
	return ids, nil
}

// ListInstances 分页查询实例。普通用户只看到资源组关联的实例。
func (s *Service) ListInstances(c Caller, q ListQuery) (*InstanceList, error) {
	f := InstanceFilter{Role: q.Role, DBType: q.DBType, Tag: strings.TrimSpace(q.Tag), Q: strings.TrimSpace(q.Q), State: q.State, Env: q.Env}
	if q.Role != "" && q.Role != "master" && q.Role != "slave" {
		return nil, bad("角色只能是 master 或 slave")
	}
	if q.State != "" && q.State != "disabled" && q.State != "enabled" {
		return nil, bad("状态只能是 disabled 或 enabled")
	}
	if err := validEnv(q.Env); err != nil {
		return nil, err
	}
	switch q.Status {
	case "1":
		t := true
		f.Enabled = &t
	case "0":
		t := false
		f.Enabled = &t
	case "":
	default:
		return nil, bad("状态只能是 1 或 0")
	}
	if q.PageSize <= 0 {
		q.PageSize = 20
	}
	if q.PageSize > 200 {
		q.PageSize = 200
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	f.Limit, f.Offset = q.PageSize, (q.Page-1)*q.PageSize
	scope, err := s.scopeOf(c)
	if err != nil {
		return nil, err
	}
	list, total, err := s.repo.ListInstances(f, scope)
	if err != nil {
		return nil, err
	}
	out := &InstanceList{Total: total, Items: make([]InstanceItem, 0, len(list))}
	ids := make([]uint, 0, len(list))
	for i := range list {
		ids = append(ids, list[i].ID)
	}
	exts := s.extOf(ids)
	for i := range list {
		it := s.toItem(&list[i])
		if x, ok := exts[list[i].ID]; ok {
			it.Disabled, it.DisabledBy, it.Env, it.EnvLabel, it.EnvProd = x.Disabled, x.DisabledBy, x.Env, instext.EnvLabel(x.Env), instext.EnvIsProd(x.Env)
			if x.Disabled && x.DisabledAt != nil {
				it.DisabledAt = x.DisabledAt.Format("2006-01-02 15:04:05")
			}
		}
		out.Items = append(out.Items, it)
	}
	out.ExtReady = s.instanceExtReady()
	if s.ext != nil {
		if st, err := s.ext.InstanceStats(scope); err == nil {
			out.Stats = &st
		}
	}
	return out, nil
}

// InstanceChoice 是各页面顶部实例下拉里的一项。Supported 表示该页面功能支持这个实例类型。
type InstanceChoice struct {
	ID          uint   `json:"id"`
	Name        string `json:"instance_name"`
	Role        string `json:"type"`
	DBType      string `json:"db_type"`
	DBTypeLabel string `json:"db_type_label"`
	Supported   bool   `json:"supported"`
}

// Choices 返回当前用户可见的实例（下拉用）。feature 取 sessions / databases / accounts / params / list。
func (s *Service) Choices(c Caller, feature string) ([]InstanceChoice, error) {
	var supports func(string) bool
	switch feature {
	case "sessions":
		supports = supportsSession
	case "databases":
		supports = supportsDatabase
	case "accounts":
		supports = supportsAccount
	case "params":
		supports = supportsParam
	case "list":
		supports = func(string) bool { return true }
	default:
		return nil, bad("未知的页面")
	}
	scope, err := s.scopeOf(c)
	if err != nil {
		return nil, err
	}
	list, err := s.repo.BriefInstances(scope)
	if err != nil {
		return nil, err
	}
	out := make([]InstanceChoice, 0, len(list))
	for _, in := range list {
		out = append(out, InstanceChoice{ID: in.ID, Name: in.InstanceName, Role: in.Type, DBType: in.DBType, DBTypeLabel: dbTypeLabel(in.DBType), Supported: supports(in.DBType)})
	}
	return out, nil
}

// FormOptions 是实例表单的可选项。
type FormOptions struct {
	InstanceOptions
	DBTypes  []Option    `json:"db_types"`
	Envs     []EnvOption `json:"envs"`
	ExtReady bool        `json:"ext_ready"` // 旁路表 dbt_instance_ext 已建：可以设置环境、停用实例
	// DefaultPorts 是 PostgreSQL / MongoDB / ClickHouse 新建实例时的缺省端口（其它类型的缺省端口由前端自带）。
	DefaultPorts map[string]int `json:"default_ports,omitempty"`
}

// FormOptions 返回页面表单 / 筛选用的选项。
func (s *Service) FormOptions() (*FormOptions, error) {
	return s.formOptions()
}

// formOptions 是含全部标签的选项（保存时校验标签 ID 用）。
func (s *Service) formOptions() (*FormOptions, error) {
	o, err := s.repo.InstanceOptions()
	if err != nil {
		return nil, err
	}
	out := &FormOptions{InstanceOptions: *o, DBTypes: dbTypeLabels, Envs: EnvOptions(), ExtReady: s.instanceExtReady(), DefaultPorts: defaultPorts}
	if out.Groups == nil {
		out.Groups = []GroupOption{}
	}
	if out.Tags == nil {
		out.Tags = []TagOption{}
	}
	if out.Tunnels == nil {
		out.Tunnels = []TunnelOption{}
	}
	return out, nil
}

// ---- 新建 / 编辑 ----

// InstanceInput 是实例表单。Password 为空：新建时除免认证类型外必填，编辑时表示不修改。
type InstanceInput struct {
	Name        string  `json:"instance_name"`
	Role        string  `json:"type"`
	DBType      string  `json:"db_type"`
	Mode        string  `json:"mode"`
	Host        string  `json:"host"`
	Port        int     `json:"port"`
	User        string  `json:"user"`
	Password    string  `json:"password"`
	IsSSL       bool    `json:"is_ssl"`
	VerifySSL   bool    `json:"verify_ssl"`
	IsEnable    bool    `json:"is_enable"`
	DBName      string  `json:"db_name"`
	ShowRegex   string  `json:"show_db_name_regex"`
	DeniedRegex string  `json:"denied_db_name_regex"`
	Charset     string  `json:"charset"`
	ServiceName string  `json:"service_name"`
	SID         string  `json:"sid"`
	TunnelID    uint    `json:"tunnel_id"`
	GroupIDs    []uint  `json:"group_ids"`
	TagIDs      []uint  `json:"tag_ids"`
	Env         *string `json:"env"` // prod / test / 空；nil 表示不修改；存旁路表 dbt_instance_ext
}

// noAuthTypes 是允许不填用户名/密码的类型。
var noAuthTypes = map[string]bool{"redis": true, "memcached": true, "mongo": true, "elasticsearch": true, "opensearch": true}

var (
	hostNameRe = regexp.MustCompile(`^[A-Za-z0-9._:\-\[\]]{1,200}$`)
	charsetRe  = regexp.MustCompile(`^[A-Za-z0-9_]{0,20}$`)
)

// checkEndpoint 校验连接相关字段（保存与测试连接共用）。
func checkEndpoint(in *InstanceInput) error {
	if !validDBType(in.DBType) {
		return bad("不支持的数据库类型")
	}
	in.Host = strings.TrimSpace(in.Host)
	if !hostNameRe.MatchString(in.Host) {
		return bad("HOST 不合法（只能是 IP 或主机名）")
	}
	if in.Port < 1 || in.Port > 65535 {
		return bad("PORT 需要在 1 到 65535 之间")
	}
	var err error
	if in.User, err = cleanText("用户名", in.User, 64, !noAuthTypes[in.DBType]); err != nil {
		return err
	}
	if len(in.Password) > 128 || strings.ContainsRune(in.Password, 0) {
		return bad("密码过长（最多 128 个字符）")
	}
	if in.DBName, err = cleanText("默认数据库", in.DBName, 64, false); err != nil {
		return err
	}
	if !charsetRe.MatchString(in.Charset) {
		return bad("字符集只能由字母、数字、下划线组成，最长 20 位")
	}
	return nil
}

func compileOK(label, re string) error {
	if len(re) > 1024 {
		return bad(fmt.Sprintf("%s过长（最多 %d 个字符）", label, 1024))
	}
	if re == "" {
		return nil
	}
	if _, err := regexp.Compile(re); err != nil {
		return bad(fmt.Sprintf("%s不是合法的正则表达式：%s", label, err.Error()))
	}
	return nil
}

func (s *Service) buildWrite(in InstanceInput, id uint) (InstanceWrite, *FormOptions, error) {
	var w InstanceWrite
	var err error
	if in.Name, err = cleanText("实例名称", in.Name, 50, true); err != nil {
		return w, nil, err
	}
	if in.Role != "master" && in.Role != "slave" {
		return w, nil, bad("角色只能是 master（主库）或 slave（从库）")
	}
	if in.Mode != "" && in.Mode != "standalone" && in.Mode != "cluster" {
		return w, nil, bad("模式只能是 standalone 或 cluster")
	}
	if err := checkEndpoint(&in); err != nil {
		return w, nil, err
	}
	if id == 0 && in.Password == "" && !noAuthTypes[in.DBType] {
		return w, nil, bad("请填写密码")
	}
	if err := compileOK("显示的库正则", in.ShowRegex); err != nil {
		return w, nil, err
	}
	if err := compileOK("隐藏的库正则", in.DeniedRegex); err != nil {
		return w, nil, err
	}
	if in.Env != nil {
		if err := validEnv(*in.Env); err != nil {
			return w, nil, err
		}
		if *in.Env != "" && !s.instanceExtReady() {
			return w, nil, errExtMissing("设置实例环境", "070_instance_ext.sql")
		}
	}
	if len([]rune(in.ServiceName)) > 50 || len([]rune(in.SID)) > 50 {
		return w, nil, bad("service_name / sid 最长 50 个字符")
	}
	taken, err := s.repo.InstanceNameTaken(in.Name, id)
	if err != nil {
		return w, nil, err
	}
	if taken {
		return w, nil, conflict(fmt.Sprintf("实例名称已存在：%s", in.Name))
	}
	opts, err := s.formOptions()
	if err != nil {
		return w, nil, err
	}
	groupIDs, err := pickIDs("资源组", in.GroupIDs, func(id uint) bool {
		for _, g := range opts.Groups {
			if g.ID == id {
				return true
			}
		}
		return false
	})
	if err != nil {
		return w, nil, err
	}
	tagIDs, err := pickIDs("标签", in.TagIDs, func(id uint) bool {
		for _, t := range opts.Tags {
			if t.ID == id {
				return true
			}
		}
		return false
	})
	if err != nil {
		return w, nil, err
	}
	// 资源组可以一个都不选：实例先登记起来（比如只做慢日志采集 / 巡检，或还没想好给谁用），之后再关联。
	// 没有资源组的实例，普通用户在各业务页面里看不到；超级用户与拥有“查询所有实例”权限的用户不受影响。
	w = InstanceWrite{Name: in.Name, Role: in.Role, DBType: in.DBType, Mode: in.Mode, Host: in.Host, Port: in.Port, User: in.User,
		IsSSL: in.IsSSL, VerifySSL: in.VerifySSL, IsEnable: in.IsEnable, DBName: in.DBName, ShowRegex: in.ShowRegex,
		DeniedRegex: in.DeniedRegex, Charset: in.Charset, GroupIDs: groupIDs, TagIDs: tagIDs}
	if in.Password != "" {
		pw := in.Password
		w.Password = &pw
	} else if id == 0 {
		empty := ""
		w.Password = &empty
	}
	if in.DBType == "oracle" {
		sn, sid := strings.TrimSpace(in.ServiceName), strings.TrimSpace(in.SID)
		w.ServiceName, w.SID = &sn, &sid
	}
	if in.TunnelID != 0 {
		ok := false
		for _, t := range opts.Tunnels {
			ok = ok || t.ID == in.TunnelID
		}
		if !ok {
			return w, nil, bad("所选 SSH 隧道不存在")
		}
		tid := in.TunnelID
		w.TunnelID = &tid
	}
	return w, opts, nil
}

// pickIDs 去重并校验每个 ID 都存在。
func pickIDs(label string, ids []uint, exists func(uint) bool) ([]uint, error) {
	var out []uint
	seen := map[uint]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !exists(id) {
			return nil, bad(fmt.Sprintf("所选%s不存在（id=%d）", label, id))
		}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// SaveInstance 新建（id=0）或更新实例。
func (s *Service) SaveInstance(c Caller, id uint, in InstanceInput) (*InstanceItem, error) {
	var old *model.Instance
	if id != 0 {
		var err error
		if old, err = s.repo.GetInstance(id); err != nil {
			return nil, err
		}
		if old == nil {
			return nil, notFound("实例不存在")
		}
	}
	w, opts, err := s.buildWrite(in, id)
	if err != nil {
		return nil, err
	}
	savedID, err := s.repo.SaveInstance(id, w)
	if isDupEntry(err) { // 并发创建/改名时 InstanceNameTaken 之后唯一索引才拦住，按重名处理而不是 500
		return nil, conflict(fmt.Sprintf("实例名称已存在：%s", w.Name))
	}
	if err != nil {
		return nil, err
	}
	action := "新增实例" // i18n-ignore 审计动作名，存库的枚举
	if id != 0 {
		action = "修改实例" // i18n-ignore 审计动作名，存库的枚举
	}
	var gn, tn []string
	for _, g := range opts.Groups {
		for _, gid := range w.GroupIDs {
			if g.ID == gid {
				gn = append(gn, g.Name)
			}
		}
	}
	for _, t := range opts.Tags {
		for _, tid := range w.TagIDs {
			if t.ID == tid {
				tn = append(tn, t.Code)
			}
		}
	}
	pw := ""
	if w.Password != nil && *w.Password != "" {
		pw = "，已设置新密码" // i18n-ignore 审计详情，存库的文本不翻译
	}
	if in.Env != nil && s.instanceExtReady() { // 环境存旁路表；失败只记日志，实例本身已保存
		if cur := s.extOf([]uint{savedID})[savedID]; cur.Env != *in.Env {
			if err := s.ext.SetInstanceEnv(savedID, *in.Env, s.now()); err != nil {
				slog.Error("保存实例环境失败", "instance", savedID, "err", err)
				pw += "，环境保存失败：" + err.Error() // i18n-ignore 审计详情，存库的文本不翻译
			} else {
				pw += fmt.Sprintf("，环境：%s -> %s", nonEmptyStr(instext.EnvLabel(cur.Env), "未设置"), nonEmptyStr(instext.EnvLabel(*in.Env), "未设置")) // i18n-ignore 审计详情，存库的文本不翻译
			}
		}
	}
	s.audit(c, action, fmt.Sprintf("实例：%s(id=%d)，类型：%s/%s，地址：%s:%d，账号：%s，慢日志采集：%v，资源组：[%s]，标签：[%s]%s", // i18n-ignore 审计详情，存库的文本不翻译
		w.Name, savedID, w.Role, w.DBType, w.Host, w.Port, w.User, w.IsEnable, strings.Join(gn, ","), strings.Join(tn, ","), pw))
	saved, err := s.repo.GetInstance(savedID)
	if err != nil || saved == nil {
		return nil, err
	}
	it := s.toItem(saved)
	if x, ok := s.extOf([]uint{savedID})[savedID]; ok {
		it.Disabled, it.Env, it.EnvLabel, it.EnvProd = x.Disabled, x.Env, instext.EnvLabel(x.Env), instext.EnvIsProd(x.Env)
	}
	return &it, nil
}

func nonEmptyStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// SetInstanceEnable 开关慢日志采集（dbt_sql_instance.is_enable）。
func (s *Service) SetInstanceEnable(c Caller, id uint, enable bool) error {
	inst, err := s.visibleInstance(c, id)
	if err != nil {
		return err
	}
	if err := s.repo.SetInstanceEnable(id, enable); err != nil {
		return err
	}
	s.audit(c, "切换慢日志采集", fmt.Sprintf("实例：%s，慢日志采集：%v -> %v", instLabel(inst), inst.IsEnable, enable)) // i18n-ignore 审计，存库的文本不翻译
	return nil
}

// InstanceRefs 返回删除前检查的结果：哪些记录引用了该实例。
func (s *Service) InstanceRefs(c Caller, id uint) ([]Ref, error) {
	if _, err := s.visibleInstance(c, id); err != nil {
		return nil, err
	}
	refs, err := s.repo.InstanceRefs(id)
	if err != nil {
		return nil, err
	}
	if refs == nil {
		refs = []Ref{}
	}
	return refs, nil
}

// DeleteInstance 删除实例。被工单、权限、归档配置等引用时拒绝，并说明引用了什么。
func (s *Service) DeleteInstance(c Caller, id uint, confirm bool) error {
	inst, err := s.visibleInstance(c, id)
	if err != nil {
		return err
	}
	if !confirm {
		return bad("请先确认删除")
	}
	refs, err := s.repo.InstanceRefs(id)
	if err != nil {
		return err
	}
	var parts, zhParts []string // 给用户看的（按请求语言）与写审计的（中文）
	for _, r := range refs {
		if !r.Blocking {
			continue
		}
		n := fmt.Sprint(r.Count)
		if r.Capped {
			n += "+"
		}
		parts = append(parts, refPart(c.Lang, r, n))
		zhParts = append(zhParts, refPart(i18n.ZH, r, n))
	}
	if len(parts) > 0 {
		msg := c.tr("该实例仍被引用，不能删除：{parts}。可以改为停用慢日志采集并取消资源组关联，使它不再出现在业务页面里", "parts", strings.Join(parts, c.tr("；")))
		s.auditRefused(c, "删除实例", inst, "仍被引用："+strings.Join(zhParts, "；")) // i18n-ignore 审计，存库的文本不翻译
		return conflict(msg)
	}
	if err := s.repo.DeleteInstance(id); err != nil {
		return err
	}
	s.audit(c, "删除实例", fmt.Sprintf("实例：%s，地址：%s:%d，类型：%s", instLabel(inst), inst.Host, inst.Port, inst.DBType)) // i18n-ignore 审计，存库的文本不翻译
	return nil
}

// refPart 是删除前检查里“某类引用 N 条（处理建议）”的一段说明，按 lang 输出（中文与旧格式一致）。
func refPart(lang string, r Ref, n string) string {
	label := i18n.TP(lang, r.Label)
	if r.Hint == "" {
		return i18n.T(lang, "{label} {n} 条", "label", label, "n", n)
	}
	return i18n.T(lang, "{label} {n} 条（{hint}）", "label", label, "n", n, "hint", i18n.TP(lang, r.Hint))
}

// ---- 测试连接 ----

// TestResult 是测试连接的结果。
type TestResult struct {
	Endpoint  string `json:"endpoint"`
	DBType    string `json:"db_type"`
	Version   string `json:"version"`
	LatencyMs int64  `json:"latency_ms"`
}

// TestInstance 用已保存的配置测试连接。
func (s *Service) TestInstance(ctx context.Context, c Caller, id uint) (*TestResult, error) {
	inst, err := s.visibleInstance(c, id)
	if err != nil {
		return nil, err
	}
	ci, err := s.connInfo(inst)
	if err != nil {
		return nil, err
	}
	res, err := s.probe(ctx, ci)
	s.auditTest(c, instLabel(inst), ci, res, err)
	return res, err
}

// auditTest 为一次真正发起的测试连接写审计（成功 / 失败都记）。只写实例名、地址与结果，不含密码（驱动报错里若带了密码也抹掉）。
func (s *Service) auditTest(c Caller, label string, ci ConnInfo, res *TestResult, err error) {
	outcome := "成功" // i18n-ignore 审计详情，存库的文本不翻译
	if res != nil {
		outcome = fmt.Sprintf("成功（%s，%d ms）", res.Version, res.LatencyMs) // i18n-ignore 审计详情，存库的文本不翻译
	}
	if err != nil {
		msg := err.Error()
		if ci.Password != "" {
			msg = strings.ReplaceAll(msg, ci.Password, "***")
		}
		outcome = "失败：" + msg // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, "测试连接", fmt.Sprintf("实例：%s，地址：%s:%d，类型：%s，结果：%s", label, ci.Host, ci.Port, ci.DBType, outcome)) // i18n-ignore 审计，存库的文本不翻译
}

// TestForm 用表单里的配置测试连接；编辑已有实例（id>0）时密码留空沿用已保存的密码，隧道按表单选择。
func (s *Service) TestForm(ctx context.Context, c Caller, id uint, in InstanceInput) (*TestResult, error) {
	if err := checkEndpoint(&in); err != nil {
		return nil, err
	}
	ci := ConnInfo{DBType: in.DBType, Host: in.Host, Port: in.Port, User: in.User, Password: in.Password, DBName: in.DBName,
		Charset: in.Charset, SSL: in.IsSSL, VerifySSL: in.VerifySSL, ServiceName: strings.TrimSpace(in.ServiceName), SID: strings.TrimSpace(in.SID)}
	label := in.Name
	if id != 0 {
		inst, err := s.visibleInstance(c, id)
		if err != nil {
			return nil, err
		}
		label = instLabel(inst)
		if in.Password == "" {
			saved, err := s.connInfo(inst)
			if err != nil {
				return nil, err
			}
			ci.Password = saved.Password
		}
	} else if in.Password == "" && !noAuthTypes[in.DBType] {
		return nil, bad("请填写密码")
	}
	if in.TunnelID != 0 {
		t, err := s.repo.TunnelInfo(in.TunnelID)
		if err != nil {
			return nil, err
		}
		if t == nil {
			return nil, bad("所选 SSH 隧道不存在")
		}
		ci.Tunnel = t
	}
	if label == "" {
		label = ci.Host
	}
	res, err := s.probe(ctx, ci)
	s.auditTest(c, label, ci, res, err)
	return res, err
}

func (s *Service) probe(ctx context.Context, ci ConnInfo) (*TestResult, error) {
	start := s.now()
	res := &TestResult{Endpoint: fmt.Sprintf("%s:%d", ci.Host, ci.Port), DBType: ci.DBType}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if mysqlLike[ci.DBType] {
		t, err := s.dialer.Dial(cctx, ci)
		if err != nil {
			return nil, newErr(502, "connect_failed", fmt.Sprintf("无法连接实例：%s", err.Error()))
		}
		defer t.Close()
		r, err := t.Query(cctx, "SELECT VERSION() AS v")
		if err != nil {
			return nil, newErr(502, "connect_failed", fmt.Sprintf("已连上但查询失败：%s", err.Error()))
		}
		if recs := r.Records(); len(recs) > 0 {
			res.Version = recs[0].Str("v")
		}
	} else {
		v, err := s.dialer.Probe(cctx, ci)
		if err != nil && strings.Contains(err.Error(), "暂不支持") { // i18n-ignore 与引擎 / 拨号层返回的中文文案做匹配
			return nil, unsupported(err.Error())
		}
		if err != nil {
			return nil, newErr(502, "connect_failed", fmt.Sprintf("无法连接实例：%s", err.Error()))
		}
		res.Version = v
	}
	res.LatencyMs = s.now().Sub(start).Milliseconds()
	return res, nil
}

// ---- Binlog 清理（仅 mysql）----

// BinlogFile 是 SHOW BINARY LOGS 的一行。
type BinlogFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// PurgePlan 是清理预览：将要删除哪些文件、保留哪些。
type PurgePlan struct {
	To          string   `json:"to"`           // PURGE BINARY LOGS TO 的目标，该文件及之后的都保留
	Purge       []string `json:"purge"`        // 将被删除的文件
	PurgeSize   int64    `json:"purge_size"`   // 将释放的字节数
	Keep        int      `json:"keep"`         // 保留的文件个数
	DumpThreads int      `json:"dump_threads"` // 当前连着的从库（Binlog Dump 线程）数
	SQL         string   `json:"sql"`
	// Replicas 是连着的从库及其读取进度（只含能识别出来的；平台已登记的从库会连上去读 SHOW REPLICA STATUS）。
	Replicas []ReplicaInfo `json:"replicas"`
	// Blocked 为 true 表示清理目标越过了某个从库的读取进度，会让复制中断：预览里提示，执行时拒绝。
	Blocked     bool     `json:"blocked"`
	BlockReason string   `json:"block_reason,omitempty"` // 按请求语言生成（Caller.Lang）
	Warnings    []string `json:"warnings"`               // 同上
	blockZh     string   // BlockReason 的中文原文，写审计用
}

func (s *Service) listBinlogs(se *session) ([]BinlogFile, error) {
	r, err := se.query("SHOW BINARY LOGS")
	if err != nil {
		return nil, wrapTarget(err)
	}
	var out []BinlogFile
	for _, rec := range r.Records() {
		out = append(out, BinlogFile{Name: rec.Str("log_name"), Size: rec.Int("file_size")})
	}
	return out, nil
}

// ListBinlogs 列出实例的 binlog 文件。
func (s *Service) ListBinlogs(ctx context.Context, c Caller, id uint) ([]BinlogFile, error) {
	se, err := s.open(ctx, c, id, supportsBinlogPurge, "Binlog 清理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	files, err := s.listBinlogs(se)
	if files == nil {
		files = []BinlogFile{}
	}
	return files, err
}

// planPurge 生成清理预览；lang 是请求语言，只影响落进响应的提示文字（BlockReason / Warnings / 从库错误说明）。
func (s *Service) planPurge(se *session, to, lang string) (*PurgePlan, error) {
	if !binlogNameRe.MatchString(to) {
		return nil, bad("binlog 文件名不合法")
	}
	files, err := s.listBinlogs(se)
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, f := range files {
		if f.Name == to {
			idx = i
		}
	}
	if idx < 0 {
		return nil, notFound("所选 binlog 文件不存在（可能已被清理），请刷新后重试")
	}
	if idx == 0 {
		return nil, bad("所选文件已经是最早的 binlog，没有可清理的内容")
	}
	p := &PurgePlan{To: to, Keep: len(files) - idx, SQL: "PURGE BINARY LOGS TO " + quoteLit(to)}
	for _, f := range files[:idx] {
		p.Purge = append(p.Purge, f.Name)
		p.PurgeSize += f.Size
	}
	if r, err := se.query("SELECT COUNT(*) AS n FROM information_schema.processlist WHERE command LIKE 'Binlog Dump%'"); err == nil {
		if recs := r.Records(); len(recs) > 0 {
			p.DumpThreads = int(recs[0].Int("n"))
		}
	}
	p.Replicas, p.Warnings = []ReplicaInfo{}, []string{}
	s.checkReplicas(se, p, files, idx, lang)
	return p, nil
}

// PreviewPurge 预览清理到哪个文件：列出将被删除的文件与大小，不执行。
func (s *Service) PreviewPurge(ctx context.Context, c Caller, id uint, to string) (*PurgePlan, error) {
	se, err := s.open(ctx, c, id, supportsBinlogPurge, "Binlog 清理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	return s.planPurge(se, to, c.Lang)
}

// PurgeBinlogs 执行清理。confirm 必须等于 to（调用方在预览之后让用户再输入一次文件名确认）。
func (s *Service) PurgeBinlogs(ctx context.Context, c Caller, id uint, to, confirm string) (*PurgePlan, error) {
	se, err := s.open(ctx, c, id, supportsBinlogPurge, "Binlog 清理")
	if err != nil {
		return nil, err
	}
	defer se.close()
	if confirm == "" || confirm != to {
		return nil, bad("确认的文件名与所选 binlog 不一致")
	}
	plan, err := s.planPurge(se, to, c.Lang) // 重新取一次列表校验，不信任预览时的结果
	if err != nil {
		return nil, err
	}
	if plan.Blocked { // 越过从库读取进度会让复制中断：拒绝，并留痕
		s.audit(c, "清理Binlog"+audit.DeniedSuffix, fmt.Sprintf("实例：%s，%s，原因：%s", instLabel(se.inst), plan.SQL, plan.blockZh)) // i18n-ignore 审计，存库的文本不翻译
		return nil, conflict(plan.BlockReason)
	}
	_, execErr := se.exec("PURGE BINARY LOGS TO ?", to)
	outcome := "成功" // i18n-ignore 审计详情，存库的文本不翻译
	if execErr != nil {
		outcome = "失败：" + execErr.Error() // i18n-ignore 审计详情，存库的文本不翻译
	}
	s.audit(c, "清理Binlog", fmt.Sprintf("实例：%s，%s，将删除 %d 个文件（%s ～ %s，共 %d 字节），从库连接数：%d，结果：%s", // i18n-ignore 审计，存库的文本不翻译
		instLabel(se.inst), plan.SQL, len(plan.Purge), plan.Purge[0], plan.Purge[len(plan.Purge)-1], plan.PurgeSize, plan.DumpThreads, outcome))
	if execErr != nil {
		return nil, wrapTarget(execErr)
	}
	return plan, nil
}
