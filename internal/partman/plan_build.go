// @author: kevinkelly
// @license: Apache Licence
// @file: plan_build.go
// @time: 2026/10/28

package partman

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// planLog 是计划计算过程中的一条“检查 / 决策”记录（同原程序日志里的 Infof / Warnf）。时间由服务层在落盘时补。
type planLog struct {
	Level string // INFO / WARN / ERROR
	Msg   string
}

func (p *Plan) logf(level, format string, a ...any) {
	p.Log = append(p.Log, planLog{Level: level, Msg: fmt.Sprintf(format, a...)})
}

// dorisLiteral 是 Doris VALUES [(…), (…)) 里的边界字面量：全部是双引号字符串（bigint 毫秒、int 秒、datetime / date 文本）。
func dorisLiteral(t time.Time, cfg *Config) (string, error) {
	switch cfg.ColType {
	case ColDatetime:
		return `"` + t.Format(dtLayout) + `"`, nil
	case ColDate:
		return `"` + t.Format("2006-01-02") + `"`, nil
	case ColBigint:
		return `"` + strconv.FormatInt(t.UnixMilli(), 10) + `"`, nil
	default:
		if v := t.Unix(); v > math.MaxInt32 {
			return "", conflict("int_overflow", "边界 %s 对应的秒级时间戳 %d 超出 int 的范围，请把分区字段改为 bigint 或 datetime", t.Format(dtLayout), v)
		}
		return `"` + strconv.FormatInt(t.Unix(), 10) + `"`, nil
	}
}

var (
	dorisKeyRe     = regexp.MustCompile(`keys:\s*\[([^\]]*)\]`)
	dorisBracketRe = regexp.MustCompile(`^\s*\[\s*([^,\[\]]+?)\s*,\s*([^,\[\]]+?)\s*\)\s*$`)
	plainIdentRe   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// dorisUpper 从 SHOW PARTITIONS 的 Range 文本里读出分区上界。
//
// 容错的写法：`[types: [DATETIME]; keys: [lo]; ..types: [DATETIME]; keys: [hi]; )`（取最后一个 keys），
// 只有一个 keys，带 types 段，带引号，datetime 带小数秒，以及老版本的 `[lo, hi)`。
// 读不到、多列分区键（keys 里有逗号）、值与字段类型对不上、或解析出的时间明显不合理（早于 1971 / 晚于 2200，
// 比如把 LIST 分区的值或把秒当成毫秒）都返回 ok=false —— 调用方把它当“未知”，绝不删除。
// MAXVALUE 返回 max=true。
func dorisUpper(rng, colType string, loc *time.Location) (t time.Time, max, ok bool) {
	var raw string
	if ms := dorisKeyRe.FindAllStringSubmatch(rng, -1); len(ms) > 0 {
		raw = ms[len(ms)-1][1]
	} else if m := dorisBracketRe.FindStringSubmatch(rng); m != nil {
		raw = m[2]
	} else {
		return time.Time{}, false, false
	}
	raw = strings.TrimSpace(strings.Trim(strings.TrimSpace(raw), `'"`))
	if raw == "" || strings.Contains(raw, ",") {
		return time.Time{}, false, false
	}
	if strings.EqualFold(raw, "MAXVALUE") {
		return time.Time{}, true, true
	}
	var tm time.Time
	switch colType {
	case ColBigint, ColInt:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return time.Time{}, false, false
		}
		if colType == ColInt {
			tm = time.Unix(n, 0).In(loc)
		} else {
			tm = time.UnixMilli(n).In(loc)
		}
	case ColDatetime, ColDate:
		parsed := false
		for _, layout := range []string{dtLayout, "2006-01-02 15:04:05.999999", "2006-01-02"} {
			if v, err := time.ParseInLocation(layout, raw, loc); err == nil {
				tm, parsed = v, true
				break
			}
		}
		if !parsed {
			return time.Time{}, false, false
		}
	default:
		return time.Time{}, false, false
	}
	if y := tm.Year(); y < 1971 || y > 2200 {
		return time.Time{}, false, false
	}
	return tm, false, true
}

// dorisName 给 DROP / ADD 用的分区名：普通名字原样（与原程序一致），含特殊字符才加反引号。
func dorisName(n string) string {
	if plainIdentRe.MatchString(n) {
		return n
	}
	return quote(n)
}

// BuildPlan 校验目标表并计算要执行的语句（未分区的 MySQL 表返回 kind=init 的计划，起点为“当前窗口”）。
// 校验不通过返回 *Error（调用方原样展示给用户），不会生成任何语句。
func BuildPlan(cfg Config, st TableState, now time.Time) (*Plan, error) {
	return BuildPlanWith(cfg, st, now, InitOptions{})
}

// BuildPlanWith 同 BuildPlan，并带上“初始化”的选项。
func BuildPlanWith(cfg Config, st TableState, now time.Time, opt InitOptions) (*Plan, error) {
	tz, err := resolveTimezone(cfg.Timezone, &st)
	if err != nil {
		return nil, err
	}
	loc := tz.Loc
	if !validIdent(cfg.DBName) || !validIdent(cfg.TbName) || !validIdent(cfg.Column) {
		return nil, badReq("库名、表名或分区字段不合法")
	}
	if !contains(colTypes, cfg.ColType) || !contains(periods, cfg.Period) {
		return nil, badReq("字段类型或分区周期不合法")
	}
	if cfg.ColType == ColDate && subDay(cfg.Period) {
		return nil, badReq("date 字段只能精确到天，分区周期不能是 %s，请改用 datetime 字段或把周期改为 day 以上", cfg.Period)
	}
	doris := st.Dialect == DialectDoris
	now = now.In(loc)
	p := &Plan{Config: cfg, Now: now, Method: st.Method, Expression: st.Expression, ColumnType: st.ColumnType,
		Kind: KindMaintain, Dialect: DialectMySQL, Timezone: tz.Name, Warnings: append([]string(nil), tz.Warnings...)}
	if doris {
		p.Dialect = DialectDoris
	}
	p.logf("INFO", "时区 %s（当前 %s），方言 %s", tz.Name, offsetClock(zoneOffset(now)), p.Dialect)
	for _, w := range tz.Warnings {
		p.logf("WARN", "%s", w)
	}

	// ---- 1. 目标表必须存在、是普通表；已按该字段做了 RANGE 分区（MySQL 方言下，未分区的表走“初始化”计划） ----
	switch {
	case !st.Exists:
		return nil, conflict("table_missing", "目标表 %s 不存在", tableLabel(&cfg))
	case !st.IsBase:
		return nil, conflict("not_base_table", "%s 不是普通表（视图 / 临时表不能分区）", tableLabel(&cfg))
	}
	p.logf("INFO", "表 %s 存在，是普通表", tableLabel(&cfg))
	unpartitioned := st.Method == "" || len(st.Partitions) == 0
	if doris {
		unpartitioned = len(st.Partitions) == 0
	}
	if unpartitioned {
		if doris {
			return nil, conflict("not_partitioned", "SelectDB / Doris 不支持自动初始化未分区表，请先把 %s 建为 RANGE 分区表", tableLabel(&cfg))
		}
		p.logf("INFO", "表 %s 未分区，可初始化为 RANGE 分区表（只在手动确认后执行，定时维护不会初始化）", tableLabel(&cfg))
		return buildInitPlan(p, cfg, st, loc, now, opt)
	}
	switch {
	case !doris && st.Subpartitioned:
		return nil, conflict("subpartition", "%s 带有子分区，暂不支持维护", tableLabel(&cfg))
	case !doris && st.Method != "RANGE" && st.Method != "RANGE COLUMNS":
		return nil, conflict("bad_method", "%s 的分区方式是 %s，仅支持 RANGE / RANGE COLUMNS", tableLabel(&cfg), st.Method)
	case st.ColumnType == "":
		return nil, conflict("column_missing", "表 %s 里没有字段 %s", tableLabel(&cfg), cfg.Column)
	case !doris && normExpr(st.Expression) != strings.ToLower(cfg.Column):
		return nil, conflict("expr_mismatch", "表的分区表达式是 %s，与配置的分区字段 %s 不一致（仅支持直接按字段分区）", st.Expression, cfg.Column)
	case !strings.EqualFold(st.ColumnType, cfg.ColType):
		return nil, conflict("type_mismatch", "字段类型不匹配：配置为 %s，表里字段 %s 实际是 %s", cfg.ColType, cfg.Column, st.ColumnType)
	case !doris && (cfg.ColType == ColDatetime || cfg.ColType == ColDate) && st.Method != "RANGE COLUMNS":
		return nil, conflict("bad_method", "%s 字段需要 RANGE COLUMNS 分区，表现在是 %s", cfg.ColType, st.Method)
	}
	if doris {
		if k := normExpr(st.PartitionKey); k != "" && k != strings.ToLower(cfg.Column) {
			p.Warnings = append(p.Warnings, fmt.Sprintf("表的分区键是 %s，与配置的分区字段 %s 不一致，请确认配置", st.PartitionKey, cfg.Column))
		}
	}

	// ---- 2. 解析现有分区 ----
	seen := map[string]bool{}
	var lastBounded *Partition
	for i, rp := range st.Partitions {
		if !validIdent(rp.Name) {
			return nil, conflict("bad_partition_name", "分区名 %s 不在允许的字符范围内，拒绝处理", qstr(rp.Name))
		}
		pt := Partition{Name: rp.Name, Desc: rp.Desc}
		switch {
		case doris:
			t, max, ok := dorisUpper(rp.Desc, cfg.ColType, loc)
			switch {
			case !ok:
				pt.Unknown = true
				p.Warnings = append(p.Warnings, fmt.Sprintf("分区 %s 的 Range 读不到上界（%s），按“未知”处理：不会删除它，也不参与续建的边界计算；若计划里的新增被数据库拒绝，请人工核对该分区", rp.Name, clip(strings.TrimSpace(rp.Desc), 120)))
			case max:
				pt.MaxValue = true
			default:
				pt.Bound = t
			}
		case strings.EqualFold(strings.TrimSpace(rp.Desc), "MAXVALUE"):
			if i != len(st.Partitions)-1 {
				return nil, conflict("bad_partition", "MAXVALUE 分区 %s 不在最后一个位置", rp.Name)
			}
			pt.MaxValue = true
		default:
			b, err := parseBound(rp.Desc, cfg.ColType, loc)
			if err != nil {
				return nil, conflict("bad_bound", "分区 %s：%v", rp.Name, err)
			}
			pt.Bound = b
			if lastBounded != nil && !b.After(lastBounded.Bound) {
				return nil, conflict("bad_partition", "分区 %s 的边界不大于前一个分区，无法判断顺序", rp.Name)
			}
		}
		seen[strings.ToLower(rp.Name)] = true
		p.Existing = append(p.Existing, pt)
		if !doris && !pt.MaxValue {
			lastBounded = &p.Existing[len(p.Existing)-1]
		}
	}
	if doris {
		// SHOW PARTITIONS 不保证按 Range 排序：已知上界的按边界升序，MAXVALUE 其后，未知的放最后。
		rank := func(e Partition) int {
			switch {
			case e.Unknown:
				return 2
			case e.MaxValue:
				return 1
			}
			return 0
		}
		sort.SliceStable(p.Existing, func(i, j int) bool {
			a, b := p.Existing[i], p.Existing[j]
			if ra, rb := rank(a), rank(b); ra != rb {
				return ra < rb
			}
			return a.Bound.Before(b.Bound)
		})
		for i := range p.Existing {
			if e := &p.Existing[i]; !e.MaxValue && !e.Unknown {
				lastBounded = e
			}
		}
		for i := 1; i < len(p.Existing); i++ {
			a, b := p.Existing[i-1], p.Existing[i]
			if !a.MaxValue && !a.Unknown && !b.MaxValue && !b.Unknown && !b.Bound.After(a.Bound) {
				return nil, conflict("bad_partition", "分区 %s 与 %s 的上界相同，无法判断顺序", a.Name, b.Name)
			}
		}
	}
	unknown := 0
	for _, e := range p.Existing {
		if e.Unknown {
			unknown++
		}
	}
	if lastBounded == nil {
		if doris {
			return nil, conflict("no_bound", "%s 的分区都读不到可用的上界（Range 无法解析或只有 MAXVALUE），无法续建与清理，请核对分区字段与字段类型", tableLabel(&cfg))
		}
		return nil, conflict("no_bound", "%s 只有 MAXVALUE 分区，没有可以参照的边界，请先手工建立初始分区", tableLabel(&cfg))
	}
	hasMax := false
	for _, e := range p.Existing {
		hasMax = hasMax || e.MaxValue
	}
	if doris && hasMax {
		return nil, conflict("max_partition", "%s 含 MAXVALUE 分区，SelectDB / Doris 不能在它之后追加分区，请先手工调整分区", tableLabel(&cfg))
	}
	p.logf("INFO", "表已分区（%s），现有分区 %d 个，最后一个带边界的分区 %s 上界 %s", methodLabel(st, doris), len(p.Existing), lastBounded.Name, lastBounded.Bound.Format(dtLayout))
	if unknown > 0 {
		p.logf("WARN", "有 %d 个分区读不到上界，按“未知”处理：不删除、不参与续建的边界计算", unknown)
	}
	if !doris {
		hasMax = p.Existing[len(p.Existing)-1].MaxValue
	}
	n := cfg.Interval
	if n < 1 {
		n = 1
		p.Warnings = append(p.Warnings, "配置的周期跨度为 0，按 1 处理（请在编辑里改成 1 以上）")
	}
	lastEnd := lastBounded.Bound
	if cfg.Period == "month" || cfg.Period == "year" {
		if lastEnd.Day() != 1 || (cfg.Period == "year" && lastEnd.Month() != 1) ||
			lastEnd.Hour() != 0 || lastEnd.Minute() != 0 || lastEnd.Second() != 0 {
			return nil, conflict("misaligned", "现有最后一个分区的边界 %s 没有对齐到%s初，无法按 %s 续建", lastEnd.Format(dtLayout), map[string]string{"month": "月", "year": "年"}[cfg.Period], cfg.Period)
		}
	}

	// ---- 3. 续建：保证“当前分区之后”有 PreCreate 个分区（已存在与否只看边界，不看名字） ----
	future := 0
	for _, e := range p.Existing {
		if !e.MaxValue && !e.Unknown && e.Bound.After(now) {
			future++
		}
	}
	var count int
	if future > 0 {
		// 已有包含当前时刻的分区：future 里第一个是它，其后才是“预创建”的。
		count = cfg.PreCreate - (future - 1)
	} else {
		// 现有分区都已过期：先补到包含当前时刻的那个，再预创建 PreCreate 个。
		m := 0
		for !step(lastEnd, cfg.Period, n, m).After(now) { // 第 m 个分区的起点 <= now 就需要它
			m++
			if m > maxAddPerRun {
				break
			}
		}
		count = m + cfg.PreCreate
	}
	if count > maxAddPerRun {
		return nil, conflict("too_many", "需要一次创建 %d 个分区，超过单次上限 %d：请检查配置的周期与跨度，或先手工补建部分分区", count, maxAddPerRun)
	}
	type slot struct {
		name       string
		start, end time.Time
		lit, lo    string
	}
	var slots []slot
	for i := 0; i < count; i++ {
		s, e := step(lastEnd, cfg.Period, n, i), step(lastEnd, cfg.Period, n, i+1)
		name := partName(s, e, cfg.Period)
		if seen[strings.ToLower(name)] {
			return nil, conflict("name_conflict", "要创建的分区 %s 与现有分区重名，但边界不同（不连续），请先检查表的分区是否按本配置的周期建立", name)
		}
		seen[strings.ToLower(name)] = true
		var lit, lo string
		var err error
		if doris {
			if lit, err = dorisLiteral(e, &cfg); err == nil {
				lo, err = dorisLiteral(s, &cfg)
			}
		} else {
			lit, err = literal(e, &cfg, &st)
		}
		if err != nil {
			return nil, err
		}
		slots = append(slots, slot{name, s, e, lit, lo})
	}
	p.logf("INFO", "当前时刻 %s，当前分区之后已有 %d 个分区，预创建目标 %d 个", now.Format(dtLayout), maxInt(future-1, 0), cfg.PreCreate)
	switch {
	case len(slots) == 0:
		p.logf("INFO", "nothing to add（未来分区已齐全）")
	case doris:
		for _, s := range slots {
			p.Adds = append(p.Adds, Statement{Kind: "add", Partitions: []string{s.name}, Bound: s.end.Format(dtLayout),
				SQL: fmt.Sprintf("ALTER TABLE %s.%s ADD PARTITION %s VALUES [(%s), (%s))", quote(cfg.DBName), quote(cfg.TbName), dorisName(s.name), s.lo, s.lit)})
		}
	case hasMax:
		maxName := p.Existing[len(p.Existing)-1].Name
		var parts, names []string
		for _, s := range slots {
			parts = append(parts, fmt.Sprintf("PARTITION %s VALUES LESS THAN (%s)", quote(s.name), s.lit))
			names = append(names, s.name)
		}
		parts = append(parts, fmt.Sprintf("PARTITION %s VALUES LESS THAN (MAXVALUE)", quote(maxName)))
		names = append(names, maxName)
		p.Reorganize = &Statement{Kind: "reorganize", Partitions: names, Bound: slots[len(slots)-1].end.Format(dtLayout), Heavy: true,
			SQL: fmt.Sprintf("ALTER TABLE %s.%s REORGANIZE PARTITION %s INTO (%s)", quote(cfg.DBName), quote(cfg.TbName), quote(maxName), strings.Join(parts, ", "))}
		p.Warnings = append(p.Warnings, fmt.Sprintf("表里有 MAXVALUE 分区 %s，续建需要 REORGANIZE PARTITION：会重写该分区里已有的数据，数据量大时耗时较长、期间持有元数据锁，需要手动确认；定时维护不会执行它", maxName))
	default:
		for _, s := range slots {
			p.Adds = append(p.Adds, Statement{Kind: "add", Partitions: []string{s.name}, Bound: s.end.Format(dtLayout),
				SQL: fmt.Sprintf("ALTER TABLE %s.%s ADD PARTITION (PARTITION %s VALUES LESS THAN (%s))", quote(cfg.DBName), quote(cfg.TbName), quote(s.name), s.lit)})
		}
	}
	if len(slots) > 0 {
		names := make([]string, len(slots))
		for i, s := range slots {
			names[i] = s.name
		}
		p.logf("INFO", "准备新增分区: %s", strings.Join(names, ", "))
	}

	// ---- 4. 清理：已经完全过去的分区（边界 <= 现在）里，只保留最近 Retain 个；读不到上界的分区永不删除 ----
	if cfg.Retain > 0 {
		var hist []Partition
		for _, e := range p.Existing {
			if !e.MaxValue && !e.Unknown && !e.Bound.After(now) {
				hist = append(hist, e)
			}
		}
		if extra := len(hist) - cfg.Retain; extra > 0 {
			var names []string
			for _, e := range hist[:extra] {
				sql := fmt.Sprintf("ALTER TABLE %s.%s DROP PARTITION %s", quote(cfg.DBName), quote(cfg.TbName), quote(e.Name))
				if doris {
					sql = fmt.Sprintf("ALTER TABLE %s.%s DROP PARTITION %s", quote(cfg.DBName), quote(cfg.TbName), dorisName(e.Name))
				}
				p.Drops = append(p.Drops, Statement{Kind: "drop", Partitions: []string{e.Name}, Bound: e.Bound.Format(dtLayout), Danger: true, SQL: sql})
				names = append(names, e.Name)
			}
			p.logf("INFO", "历史分区 %d 个，保留 %d 个，准备删除过期分区: %s", len(hist), cfg.Retain, strings.Join(names, ", "))
		} else {
			p.logf("INFO", "nothing to drop（历史分区 %d 个，未超过保留数 %d）", len(hist), cfg.Retain)
		}
	} else {
		p.Warnings = append(p.Warnings, "保留历史分区数为 0：全部历史分区都会保留，不会删除任何分区")
		p.logf("INFO", "retain<=0，跳过删除（不删除任何历史分区）")
	}
	return p, nil
}

func methodLabel(st TableState, doris bool) string {
	if doris {
		return "SHOW PARTITIONS"
	}
	return st.Method
}

// zoneOffset 返回 t 所在时区此刻的偏移秒数。
func zoneOffset(t time.Time) int {
	_, off := t.Zone()
	return off
}

// partitionEngines 是已知支持分区的存储引擎（大写）。
var partitionEngines = map[string]bool{"INNODB": true, "MYISAM": true, "MEMORY": true, "ARCHIVE": true, "ROCKSDB": true, "TOKUDB": true, "XENGINE": true}

// buildInitPlan 为未分区的 MySQL 表生成“初始化为 RANGE 分区表”的计划。窗口按原程序的网格对齐（alignDown），
// 从起点窗口一直建到“当前窗口 + preCreate”；不带 MAXVALUE 兜底分区。前置检查不通过时 Init.CanExecute=false 且没有语句。
func buildInitPlan(p *Plan, cfg Config, st TableState, loc *time.Location, now time.Time, opt InitOptions) (*Plan, error) {
	p.Kind = KindInit
	info := &InitInfo{StartMode: opt.StartMode, StartDate: strings.TrimSpace(opt.StartDate)}
	if info.StartMode == "" {
		info.StartMode = StartCurrent
	}
	p.Init = info
	add := func(code, level, format string, a ...any) {
		info.Checks = append(info.Checks, InitCheck{Code: code, Level: level, Message: fmt.Sprintf(format, a...)})
	}

	// ---- 起点 ----
	iv := maxInt(cfg.Interval, 1)
	cur := alignDown(now, cfg.Period, iv, loc)
	start := cur
	switch info.StartMode {
	case StartCurrent:
	case StartEarliest:
		switch {
		case !opt.EarliestRead:
			return nil, conflict("earliest_unavailable", "没有读取到最早数据时间，请改用“指定起始日期”")
		case !opt.HasEarliest:
			info.Notes = append(info.Notes, "表里没有数据，按“从当前窗口开始”处理")
		default:
			info.Earliest = opt.Earliest.In(loc)
			if a := alignDown(info.Earliest, cfg.Period, iv, loc); a.Before(start) {
				start = a
			}
		}
	case StartDate:
		var t time.Time
		var perr error
		for _, layout := range []string{dtLayout, "2006-01-02"} {
			if t, perr = time.ParseInLocation(layout, info.StartDate, loc); perr == nil {
				break
			}
		}
		if perr != nil {
			return nil, newErr(400, "bad_init_option", "起始日期 %s 格式不对，需要 YYYY-MM-DD", qstr(info.StartDate))
		}
		a := alignDown(t, cfg.Period, iv, loc)
		if a.After(cur) {
			return nil, newErr(400, "bad_init_option", "起始日期 %s 晚于当前窗口（%s），初始化需要从当前窗口或更早开始", t.Format("2006-01-02"), cur.Format(dtLayout))
		}
		start = a
	default:
		return nil, newErr(400, "bad_init_option", "起点方式只能是 current、earliest 或 date")
	}
	info.Start = start
	p.logf("INFO", "初始化起点方式 %s，第一个窗口起点 %s，当前窗口 %s", info.StartMode, start.Format(dtLayout), cur.Format(dtLayout))
	for _, note := range info.Notes {
		p.logf("INFO", "%s", note)
	}

	// ---- 前置检查 ----
	add("table_kind", "ok", "%s 是普通表", tableLabel(&cfg))
	switch eng := strings.TrimSpace(st.Engine); {
	case eng == "":
		add("engine", "warn", "没有读到存储引擎，无法确认是否支持分区（InnoDB 支持）")
	case partitionEngines[strings.ToUpper(eng)]:
		add("engine", "ok", "存储引擎 %s 支持分区", eng)
	default:
		add("engine", "error", "存储引擎 %s 不支持分区（支持 InnoDB、MyISAM 等）", eng)
	}
	if len(st.ForeignKeys) > 0 {
		add("foreign_key", "error", "表存在外键关系（%s）：MySQL 分区表不支持外键，请先处理外键", strings.Join(st.ForeignKeys, "；"))
	} else {
		add("foreign_key", "ok", "没有外键关系")
	}
	switch {
	case st.ColumnType == "":
		add("column", "error", "表 %s 里没有字段 %s", tableLabel(&cfg), cfg.Column)
	case !strings.EqualFold(st.ColumnType, cfg.ColType):
		add("column_type", "error", "字段类型不匹配：配置为 %s，表里字段 %s 实际是 %s", cfg.ColType, cfg.Column, st.ColumnType)
	default:
		add("column", "ok", "分区字段 %s 存在，类型 %s", cfg.Column, st.ColumnType)
	}
	var badUK []string
	for _, uk := range st.UniqueKeys {
		has := false
		for _, c := range uk.Columns {
			has = has || strings.EqualFold(c, cfg.Column)
		}
		if !has {
			badUK = append(badUK, fmt.Sprintf("%s(%s)", uk.Name, strings.Join(uk.Columns, ",")))
		}
	}
	switch {
	case len(badUK) > 0:
		add("unique_keys", "error", "分区字段 %s 必须包含在每一个唯一键（含主键）里，这些唯一键不包含它：%s", cfg.Column, strings.Join(badUK, "、"))
	case len(st.UniqueKeys) == 0:
		add("unique_keys", "ok", "表没有唯一键")
	default:
		names := make([]string, 0, len(st.UniqueKeys))
		for _, uk := range st.UniqueKeys {
			names = append(names, uk.Name)
		}
		add("unique_keys", "ok", "唯一键均包含分区字段 %s（%s）", cfg.Column, strings.Join(names, "、"))
	}

	// ---- 窗口：从起点一直到“当前窗口 + preCreate” ----
	end := step(cur, cfg.Period, iv, cfg.PreCreate)
	type win struct {
		name       string
		start, end time.Time
	}
	var wins []win
	overflow := false
	for w := start; !w.After(end); w = step(w, cfg.Period, iv, 1) {
		if len(wins) >= maxInitPartitions {
			overflow = true
			break
		}
		e := step(w, cfg.Period, iv, 1)
		wins = append(wins, win{partName(w, e, cfg.Period), w, e})
	}
	switch {
	case overflow:
		add("partition_count", "error", "需要创建的分区数超过上限 %d，请缩小数据跨度（改用“从当前窗口开始”或更近的起始日期）或把周期调大", maxInitPartitions)
	case len(wins) > warnInitPartitions:
		add("partition_count", "warn", "将创建 %d 个分区，超过 %d，分区过多会拖慢元数据操作与优化器", len(wins), warnInitPartitions)
	default:
		add("partition_count", "ok", "将创建 %d 个分区", len(wins))
	}
	if !overflow && len(wins) > 0 {
		info.PartitionCount = len(wins)
		info.First, info.Last = wins[0].name, wins[len(wins)-1].name
		info.LastBound = wins[len(wins)-1].end
	}
	info.CanExecute = true
	for _, c := range info.Checks {
		lv := "INFO"
		switch c.Level {
		case "error":
			info.CanExecute = false
			lv = "ERROR"
		case "warn":
			lv = "WARN"
		}
		p.logf(lv, "前置检查 %s：%s", c.Code, c.Message)
	}

	// ---- 语句 ----
	if info.CanExecute {
		parts := make([]string, 0, len(wins))
		names := make([]string, 0, len(wins))
		seen := map[string]bool{}
		for _, w := range wins {
			if seen[w.name] {
				return nil, conflict("name_conflict", "初始化会生成重名的分区 %s，请调整周期与跨度", w.name)
			}
			seen[w.name] = true
			lit, err := literal(w.end, &cfg, &st)
			if err != nil {
				info.CanExecute = false
				info.PartitionCount, info.First, info.Last = 0, "", ""
				add("column_type", "error", "%v", err)
				p.logf("ERROR", "前置检查 column_type：%v", err)
				return p, nil
			}
			parts = append(parts, fmt.Sprintf("PARTITION %s VALUES LESS THAN (%s)", quote(w.name), lit))
			names = append(names, w.name)
		}
		by := fmt.Sprintf("RANGE (%s)", quote(cfg.Column))
		if cfg.ColType == ColDatetime || cfg.ColType == ColDate {
			by = fmt.Sprintf("RANGE COLUMNS(%s)", quote(cfg.Column))
		}
		p.InitStmt = &Statement{Kind: "init", Partitions: names, Bound: info.LastBound.Format(dtLayout), Danger: true, Heavy: true,
			SQL: fmt.Sprintf("ALTER TABLE %s.%s PARTITION BY %s (%s)", quote(cfg.DBName), quote(cfg.TbName), by, strings.Join(parts, ", "))}
		p.logf("INFO", "表未分区，准备首次建分区，共 %d 个分区: %s ... %s", len(names), info.First, info.Last)

		p.Warnings = append(p.Warnings, "初始化会用一条 ALTER TABLE … PARTITION BY 重建整张表：数据量大时耗时很长、期间持有元数据锁并占用额外磁盘，请在业务低峰执行")
		if info.StartMode != StartEarliest {
			p.Warnings = append(p.Warnings, fmt.Sprintf("起点之前的存量数据会全部落进第一个分区 %s，之后按“保留历史分区数”清理该分区时会连同这些旧数据一起删除；需要把旧数据按周期拆开，请选择“从最早数据开始”或指定更早的起始日期", info.First))
		}
		p.Warnings = append(p.Warnings, fmt.Sprintf("没有 MAXVALUE 兜底分区：分区字段值大于等于 %s 的行无法写入，表里已有这样的数据时 ALTER 会失败", info.LastBound.Format(dtLayout)))
	}
	return p, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
