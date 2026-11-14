// @author: kevinkelly
// @license: Apache Licence
// @file: envdict.go
// @time: 2026/10/28

package instext

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

// 环境字典（deploy/sql/270_environment.sql）：实例的“环境”不再写死 prod / test，而是在系统管理里维护。
//
// 约定（与本包其余部分一致）：字典表不存在、为空或读取失败时，退回内置的 prod（生产）/ test（测试），行为与引入字典之前完全一致。
// 实例的环境值仍是字典里的 code，存在 dbt_instance_ext.env；各处只通过本文件的函数判断合法性、取显示名和“是否按生产对待”。

// EnvTable 是环境字典表名。
const EnvTable = "dbt_environment"

// EnvDef 是字典里的一个环境。
type EnvDef struct {
	Code    string `json:"code"`
	Name    string `json:"name"`
	Prod    bool   `json:"is_prod"` // 按生产对待：红色徽标、工单详情的生产提示、防火墙“仅生产”类规则的参照
	Sort    int    `json:"sort"`
	Builtin bool   `json:"builtin"` // prod / test：不可删除、编码与“是否生产”不可改
}

// 限制。
const (
	EnvCodeMax = 16
	EnvNameMax = 20
	EnvMax     = 20
)

var envCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,15}$`)

// ReservedEnvCodes 不能作为环境编码（防火墙规则里的“全部实例”用 all）。
var ReservedEnvCodes = map[string]bool{"all": true, "none": true}

func builtinEnvs() []EnvDef {
	return []EnvDef{
		{Code: EnvProd, Name: "生产", Prod: true, Sort: 10, Builtin: true},
		{Code: EnvTest, Name: "测试", Prod: false, Sort: 20, Builtin: true},
	}
}

// EnvRow -> dbt_environment。
type EnvRow struct {
	Code      string    `gorm:"primaryKey;column:code"`
	Name      string    `gorm:"column:name"`
	IsProd    bool      `gorm:"column:is_prod"`
	SortOrder int       `gorm:"column:sort_order"`
	CreatedBy string    `gorm:"column:created_by"`
	UpdatedBy string    `gorm:"column:updated_by"`
	CreatedAt time.Time `gorm:"column:created_at"`
	UpdatedAt time.Time `gorm:"column:updated_at"`
}

func (EnvRow) TableName() string { return EnvTable }

func (r EnvRow) def() EnvDef {
	return EnvDef{Code: r.Code, Name: r.Name, Prod: r.IsProd, Sort: r.SortOrder, Builtin: r.Code == EnvProd || r.Code == EnvTest}
}

// ---- 缓存读取 ----

const envTTL = 30 * time.Second

var (
	envMu     sync.Mutex
	envDB     *gorm.DB
	envDefs   []EnvDef
	envAt     time.Time
	envInstAt time.Time
	envInst   map[uint]string // 实例 id -> 环境 code（只含已设置的）
)

// BindEnvDB 绑定读取字典用的元数据库连接（服务启动时调用一次）；没绑定时只有内置的两个环境。
func BindEnvDB(db *gorm.DB) {
	envMu.Lock()
	envDB = db
	envDefs, envAt, envInst, envInstAt = nil, time.Time{}, nil, time.Time{}
	envMu.Unlock()
}

// InvalidateEnvs 让字典与“实例 -> 环境”缓存立即失效（写字典、改实例环境后调用）。
func InvalidateEnvs() {
	envMu.Lock()
	envDefs, envAt, envInst, envInstAt = nil, time.Time{}, nil, time.Time{}
	envMu.Unlock()
}

// EnvDefs 返回全部环境（按 sort、code 排序）。字典不可用时返回内置的两个。
func EnvDefs() []EnvDef {
	envMu.Lock()
	db := envDB
	if envDefs != nil && time.Since(envAt) < envTTL {
		out := append([]EnvDef(nil), envDefs...)
		envMu.Unlock()
		return out
	}
	envMu.Unlock()
	defs := loadEnvDefs(db)
	envMu.Lock()
	envDefs, envAt = defs, time.Now()
	out := append([]EnvDef(nil), defs...)
	envMu.Unlock()
	return out
}

func loadEnvDefs(db *gorm.DB) []EnvDef {
	if db == nil {
		return builtinEnvs()
	}
	var rows []EnvRow
	if err := db.Session(&gorm.Session{NewDB: true}).Order("sort_order, code").Find(&rows).Error; err != nil || len(rows) == 0 {
		return builtinEnvs()
	}
	out := make([]EnvDef, 0, len(rows))
	have := map[string]bool{}
	for _, r := range rows {
		out = append(out, r.def())
		have[r.Code] = true
	}
	for _, b := range builtinEnvs() { // 内置环境被误删时仍然可用
		if !have[b.Code] {
			out = append(out, b)
		}
	}
	sortEnvs(out)
	return out
}

func sortEnvs(l []EnvDef) {
	sort.SliceStable(l, func(i, j int) bool {
		if l[i].Sort != l[j].Sort {
			return l[i].Sort < l[j].Sort
		}
		return l[i].Code < l[j].Code
	})
}

// EnvDefOf 取一个环境的定义；空串或不存在返回 false。
func EnvDefOf(code string) (EnvDef, bool) {
	if code == "" {
		return EnvDef{}, false
	}
	for _, d := range EnvDefs() {
		if d.Code == code {
			return d, true
		}
	}
	return EnvDef{}, false
}

// ValidEnv 判断环境取值是否合法（空表示未设置）。
func ValidEnv(e string) bool {
	if e == "" {
		return true
	}
	_, ok := EnvDefOf(e)
	return ok
}

// EnvLabel 是环境的显示名；空串返回空；字典里找不到时返回编码本身。
func EnvLabel(e string) string {
	if e == "" {
		return ""
	}
	if d, ok := EnvDefOf(e); ok {
		return d.Name
	}
	return e
}

// EnvIsProd 报告环境是否按生产对待。
func EnvIsProd(e string) bool {
	if d, ok := EnvDefOf(e); ok {
		return d.Prod
	}
	return e == EnvProd
}

// InstanceEnvOf 是一个实例的“有效环境”：实例管理里设置的环境优先，没设置时回退到 fallback（调用方由标签推断的 prod / test）。
// 读的是整张旁路表的缓存（30 秒），所以可以在每次查询 / 提交工单时调用。
func InstanceEnvOf(id uint, fallback string) string {
	envMu.Lock()
	db := envDB
	fresh := envInst != nil && time.Since(envInstAt) < envTTL
	m := envInst
	envMu.Unlock()
	if !fresh && db != nil {
		m = loadInstanceEnvs(db)
		envMu.Lock()
		envInst, envInstAt = m, time.Now()
		envMu.Unlock()
	}
	if e := m[id]; e != "" {
		return e
	}
	return fallback
}

func loadInstanceEnvs(db *gorm.DB) map[uint]string {
	out := map[uint]string{}
	if !Ready(db) {
		return out
	}
	var rows []struct {
		InstanceID uint   `gorm:"column:instance_id"`
		Env        string `gorm:"column:env"`
	}
	if err := db.Session(&gorm.Session{NewDB: true}).Table(Table).Select("instance_id, env").Where("env <> ''").Scan(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		out[r.InstanceID] = r.Env
	}
	return out
}

// ---- 维护（平台设置 › 环境设置）----

// 维护层错误。
var (
	ErrEnvTableMissing = errors.New("环境字典表 dbt_environment 不存在，请先执行 deploy/sql/270_environment.sql")
	ErrEnvNotFound     = errors.New("环境不存在")
	ErrEnvCodeTaken    = errors.New("已有同编码的环境")
	ErrEnvNameTaken    = errors.New("已有同名的环境")
	ErrEnvInUse        = errors.New("环境正被使用")
	ErrEnvBuiltin      = errors.New("内置环境不可删除")
	ErrEnvLimit        = errors.New("环境最多 20 个")
)

// EnvReady 字典表是否已建。
func EnvReady(db *gorm.DB) bool {
	return db != nil && db.Session(&gorm.Session{NewDB: true}).Migrator().HasTable(EnvTable)
}

// EnvUsage 统计各环境被多少个实例使用。
func EnvUsage(db *gorm.DB) (map[string]int, error) {
	out := map[string]int{}
	if db == nil || !Ready(db) {
		return out, nil
	}
	var rows []struct {
		Env string
		N   int
	}
	if err := db.Session(&gorm.Session{NewDB: true}).Table(Table).Select("env, COUNT(*) AS n").Where("env <> ''").Group("env").Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.Env] = r.N
	}
	return out, nil
}

// ValidateEnvInput 校验新增 / 修改的字段，返回规整后的值。
func ValidateEnvInput(code, name string, requireCode bool) (string, string, error) {
	code = strings.TrimSpace(code)
	name = strings.TrimSpace(name)
	if requireCode {
		if !envCodeRe.MatchString(code) {
			return "", "", errors.New("编码只能用小写字母开头的小写字母、数字、下划线、中划线，最长 16 位")
		}
		if ReservedEnvCodes[code] {
			return "", "", fmt.Errorf("编码 %s 是保留字，请换一个", code)
		}
	}
	if n := utf8.RuneCountInString(name); n == 0 || n > EnvNameMax {
		return "", "", errors.New("显示名称不能为空，最长 20 个字符")
	}
	return code, name, nil
}

// ListEnvRows 读出字典表的全部行（含内置补齐），按 sort、code 排序。
func ListEnvRows(db *gorm.DB) ([]EnvDef, error) {
	if !EnvReady(db) {
		return builtinEnvs(), nil
	}
	var rows []EnvRow
	if err := db.Session(&gorm.Session{NewDB: true}).Order("sort_order, code").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]EnvDef, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.def())
	}
	return out, nil
}

// CreateEnv 新增一个环境。
func CreateEnv(db *gorm.DB, row EnvRow) error {
	if !EnvReady(db) {
		return ErrEnvTableMissing
	}
	tx := db.Session(&gorm.Session{NewDB: true})
	var n int64
	tx.Model(&EnvRow{}).Count(&n)
	if n >= EnvMax {
		return ErrEnvLimit
	}
	if err := tx.Model(&EnvRow{}).Where("code = ?", row.Code).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return ErrEnvCodeTaken
	}
	if err := tx.Model(&EnvRow{}).Where("name = ?", row.Name).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return ErrEnvNameTaken
	}
	if err := tx.Create(&row).Error; err != nil {
		return err
	}
	InvalidateEnvs()
	return nil
}

// UpdateEnv 修改显示名、是否生产、排序（编码不可改；内置环境的“是否生产”不可改）。
func UpdateEnv(db *gorm.DB, code, name string, prod bool, sortOrder int, by string, at time.Time) (EnvDef, EnvDef, error) {
	if !EnvReady(db) {
		return EnvDef{}, EnvDef{}, ErrEnvTableMissing
	}
	tx := db.Session(&gorm.Session{NewDB: true})
	var row EnvRow
	if err := tx.Where("code = ?", code).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return EnvDef{}, EnvDef{}, ErrEnvNotFound
		}
		return EnvDef{}, EnvDef{}, err
	}
	old := row.def()
	var n int64
	if err := tx.Model(&EnvRow{}).Where("name = ? AND code <> ?", name, code).Count(&n).Error; err != nil {
		return old, old, err
	}
	if n > 0 {
		return old, old, ErrEnvNameTaken
	}
	row.Name, row.SortOrder, row.UpdatedBy, row.UpdatedAt = name, sortOrder, by, at
	if !old.Builtin {
		row.IsProd = prod
	}
	if err := tx.Save(&row).Error; err != nil {
		return old, old, err
	}
	InvalidateEnvs()
	return old, row.def(), nil
}

// DeleteEnv 删除环境：内置的不可删；仍有实例使用的不可删。返回被删的定义。
func DeleteEnv(db *gorm.DB, code string) (EnvDef, error) {
	if !EnvReady(db) {
		return EnvDef{}, ErrEnvTableMissing
	}
	tx := db.Session(&gorm.Session{NewDB: true})
	var row EnvRow
	if err := tx.Where("code = ?", code).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return EnvDef{}, ErrEnvNotFound
		}
		return EnvDef{}, err
	}
	def := row.def()
	if def.Builtin {
		return def, ErrEnvBuiltin
	}
	use, err := EnvUsage(db)
	if err != nil {
		return def, err
	}
	if use[code] > 0 {
		return def, ErrEnvInUse
	}
	if err := tx.Delete(&EnvRow{}, "code = ?", code).Error; err != nil {
		return def, err
	}
	InvalidateEnvs()
	return def, nil
}
