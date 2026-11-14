// @author: kevinkelly
// @license: Apache Licence
// @file: notify.go
// @time: 2026/10/28

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"dbtool/internal/model"
	"dbtool/internal/notify"
)

// 编译期检查：Store 同时是通知的渠道来源和 @ 对象目录。
var (
	_ notify.Source         = (*Store)(nil)
	_ notify.Directory      = (*Store)(nil)
	_ notify.Admin          = (*Store)(nil)
	_ notify.LogSink        = (*Store)(nil)
	_ notify.LogStore       = (*Store)(nil)
	_ notify.EnabledSetter  = (*Store)(nil)
	_ notify.ExtAdmin       = (*Store)(nil)
	_ notify.EmailAdmin     = (*Store)(nil)
	_ notify.GroupDirectory = (*Store)(nil)
)

// ---- 旁路表是否存在（缓存 30 秒；表建好后最多 30 秒生效，不用重启） ----

const tableReadyTTL = 30 * time.Second

type readyCache struct {
	mu sync.Mutex
	at time.Time
	ok bool
}

var tableReady sync.Map // 表名 -> *readyCache（全进程只有一个元数据库）

func (s *Store) hasTable(name string, tbl any) bool {
	v, _ := tableReady.LoadOrStore(name, &readyCache{})
	c := v.(*readyCache)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < tableReadyTTL {
		return c.ok
	}
	db := s.DB.Session(&gorm.Session{Logger: s.DB.Logger.LogMode(logger.Silent)})
	c.ok, c.at = db.Migrator().HasTable(tbl), time.Now()
	return c.ok
}

// NotifyTableReady 判断 dbt_notify_channel 表是否已创建（需要手动执行 deploy/sql/001_notify_channel.sql）。
func (s *Store) NotifyTableReady() bool {
	return s.DB.Migrator().HasTable(&model.NotifyChannel{})
}

// NotifyExtReady 判断 dbt_notify_channel_ext 是否已创建（deploy/sql/091_notify_ext.sql）：多资源组、值班人员、细分事件模型依赖它。
func (s *Store) NotifyExtReady() bool {
	return s.hasTable("dbt_notify_channel_ext", &model.NotifyChannelExt{})
}

// NotifyEmailReady 判断 dbt_notify_email 是否已创建（deploy/sql/210_notify_email.sql）：邮件渠道的 SMTP 配置存在这里。
func (s *Store) NotifyEmailReady() bool {
	return s.hasTable("dbt_notify_email", &model.NotifyEmail{})
}

// LogTableReady 判断 dbt_notify_log 是否已创建（deploy/sql/092_notify_log.sql）。
func (s *Store) LogTableReady() bool { return s.hasTable("dbt_notify_log", &model.NotifyLog{}) }

func hostOf(raw string) string {
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexAny(raw, "/?"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// ---- 扩展信息的编码 ----

func joinIDs(ids []uint) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatUint(uint64(id), 10))
	}
	return strings.Join(parts, ",")
}

func splitNames(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// groupIDsOf 返回渠道关联的资源组：扩展表里有就用它，否则只有 group_id 一个；空表示全局。
func groupIDsOf(groupID uint, ext *model.NotifyChannelExt) []uint {
	if ext != nil {
		if ids := notify.ParseIDs(ext.GroupIDs); len(ids) > 0 {
			return ids
		}
	}
	if groupID != 0 {
		return []uint{groupID}
	}
	return nil
}

// channelApplies 判断渠道对某个资源组是否生效：全局渠道对所有资源组生效；groupID 为 0 的事件只发给全局渠道。
func channelApplies(gids []uint, groupID uint) bool {
	if len(gids) == 0 {
		return true
	}
	for _, g := range gids {
		if g == groupID {
			return true
		}
	}
	return false
}

// channelExts 读全部渠道的扩展信息；表不存在返回空。
func (s *Store) channelExts() map[uint]model.NotifyChannelExt {
	out := map[uint]model.NotifyChannelExt{}
	if !s.NotifyExtReady() {
		return out
	}
	var rows []model.NotifyChannelExt
	if err := s.DB.Find(&rows).Error; err != nil {
		slog.Warn("读取通知渠道扩展信息失败", "err", err)
		return out
	}
	for _, r := range rows {
		out[r.ChannelID] = r
	}
	return out
}

// channelEmails 读邮件渠道的 SMTP 配置（channel_id -> 配置）；表不存在或 JSON 损坏的跳过（损坏的渠道发送时报“缺少 SMTP 配置”）。
func (s *Store) channelEmails() map[uint]*notify.EmailConfig {
	out := map[uint]*notify.EmailConfig{}
	if !s.NotifyEmailReady() {
		return out
	}
	var rows []model.NotifyEmail
	if err := s.DB.Find(&rows).Error; err != nil {
		slog.Warn("读取邮件渠道配置失败", "err", err)
		return out
	}
	for _, r := range rows {
		var c notify.EmailConfig
		if err := json.Unmarshal([]byte(r.Config), &c); err != nil {
			slog.Warn("邮件渠道配置不是合法的 JSON", "channel_id", r.ChannelID)
			continue
		}
		out[r.ChannelID] = &c
	}
	return out
}

func hasEmailRow(rows []model.NotifyChannel) bool {
	for _, r := range rows {
		if r.ChannelType == notify.TypeEmail {
			return true
		}
	}
	return false
}

func (s *Store) toChannel(row model.NotifyChannel, ext *model.NotifyChannelExt) notify.Channel {
	ch := notify.Channel{
		ID: row.ID, GroupID: row.GroupID, Name: row.Name, Type: row.ChannelType, Enabled: row.Enabled,
		WebhookURL: s.Mirage.Decrypt(row.WebhookURL), Secret: s.Mirage.Decrypt(row.Secret), Events: notify.SplitEvents(row.Events),
	}
	_ = json.Unmarshal([]byte(row.Mentions), &ch.Rules)
	if ext != nil {
		ch.GroupIDs, ch.DutyUsers, ch.EventsV2 = notify.ParseIDs(ext.GroupIDs), splitNames(ext.DutyUsers), ext.EventsV2
	}
	return ch
}

// Channels 实现 notify.Source：某个资源组生效的渠道 = 关联了该组的 + 全局的（已解密）。
// 一个渠道可以关联多个资源组（扩展表 group_ids）；没有扩展信息的渠道按 group_id 单个资源组处理，行为与升级前一致。
// dbt_resource_group 表里旧的三个 webhook 列仍然生效，但只在“该资源组没有配置同类型的新渠道”时才发，避免重复。
func (s *Store) Channels(groupID uint) ([]notify.Channel, error) {
	var out []notify.Channel
	have := map[string]bool{}
	if s.NotifyTableReady() {
		exts := s.channelExts()
		var rows []model.NotifyChannel
		q := s.DB.Where("enabled = 1").Order("id")
		if len(exts) == 0 {
			q = q.Where("group_id IN ?", []uint{groupID, 0}) // 没有扩展信息：只看 group_id，直接在库里过滤
		}
		if err := q.Find(&rows).Error; err != nil {
			return nil, err
		}
		var emails map[uint]*notify.EmailConfig
		if hasEmailRow(rows) {
			emails = s.channelEmails()
		}
		for _, r := range rows {
			var ext *model.NotifyChannelExt
			if e, ok := exts[r.ID]; ok {
				ext = &e
			}
			if !channelApplies(groupIDsOf(r.GroupID, ext), groupID) {
				continue
			}
			ch := s.toChannel(r, ext)
			ch.Email = emails[r.ID]
			out = append(out, ch)
			have[r.ChannelType] = true
		}
	}
	rg, err := s.ResourceGroupByID(groupID)
	if err != nil || rg == nil {
		return out, err
	}
	for _, l := range []struct{ typ, url string }{
		{notify.TypeDingTalk, rg.DingWebhook}, {notify.TypeFeishu, rg.FeishuWebhook}, {notify.TypeSlack, rg.SlackWebhook},
	} {
		if strings.TrimSpace(l.url) != "" && !have[l.typ] {
			out = append(out, notify.Channel{GroupID: groupID, Name: fmt.Sprintf("旧版 webhook（%s）", notify.TypeLabel(l.typ)), Type: l.typ, WebhookURL: strings.TrimSpace(l.url), Enabled: true, Legacy: true})
		}
	}
	return out, nil
}

func personOf(u model.User) notify.Person {
	return notify.Person{Username: u.Username, Display: u.Display, DingID: u.DingUserID, FeishuID: u.FeishuOpenID, SlackID: u.SlackID, Email: strings.TrimSpace(u.Email)}
}

// ResourceGroupMembers 实现 notify.GroupDirectory：属于某资源组的启用用户（邮件渠道的“所有人”规则）。
func (s *Store) ResourceGroupMembers(resourceGroupID uint) ([]notify.Person, error) {
	var rows []model.User
	err := s.DB.Raw(`
SELECT DISTINCT u.* FROM dbt_sql_users u
JOIN dbt_sql_users_resource_group ur ON ur.users_id = u.id AND ur.resourcegroup_id = ?
WHERE u.is_active = 1 ORDER BY u.id`, resourceGroupID).Scan(&rows).Error
	out := make([]notify.Person, 0, len(rows))
	for _, u := range rows {
		out = append(out, personOf(u))
	}
	return out, err
}

// UsersByName 实现 notify.Directory。
func (s *Store) UsersByName(usernames []string) ([]notify.Person, error) {
	if len(usernames) == 0 {
		return nil, nil
	}
	var rows []model.User
	if err := s.DB.Where("username IN ? AND is_active = 1", usernames).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]notify.Person, 0, len(rows))
	for _, u := range rows {
		out = append(out, personOf(u))
	}
	return out, nil
}

// AuthGroupMembers 实现 notify.Directory：属于某权限组且属于某资源组的启用用户。
func (s *Store) AuthGroupMembers(authGroupID, resourceGroupID uint) ([]notify.Person, error) {
	var rows []model.User
	err := s.DB.Raw(`
SELECT DISTINCT u.* FROM dbt_sql_users u
JOIN dbt_sql_users_groups ug ON ug.users_id = u.id AND ug.group_id = ?
JOIN dbt_sql_users_resource_group ur ON ur.users_id = u.id AND ur.resourcegroup_id = ?
WHERE u.is_active = 1 ORDER BY u.id`, authGroupID, resourceGroupID).Scan(&rows).Error
	out := make([]notify.Person, 0, len(rows))
	for _, u := range rows {
		out = append(out, personOf(u))
	}
	return out, err
}

// ---- 渠道管理（管理页面使用） ----

func (s *Store) ListChannelViews() ([]notify.ChannelView, error) {
	var rows []model.NotifyChannel
	if err := s.DB.Order("group_id, id").Find(&rows).Error; err != nil {
		return nil, err
	}
	exts := s.channelExts()
	var emails map[uint]*notify.EmailConfig
	if hasEmailRow(rows) {
		emails = s.channelEmails()
	}
	var last map[uint]notify.SendLog
	if s.LogTableReady() {
		l, err := s.LastSends()
		if err != nil {
			slog.Warn("读取通知最近发送失败", "err", err)
		} else {
			last = l
		}
	}
	out := make([]notify.ChannelView, 0, len(rows))
	for _, r := range rows {
		var ext *model.NotifyChannelExt
		if e, ok := exts[r.ID]; ok {
			ext = &e
		}
		c := s.toChannel(r, ext)
		v := notify.ChannelView{ID: r.ID, GroupID: r.GroupID, Name: r.Name, Type: r.ChannelType, WebhookHint: hostOf(c.WebhookURL),
			HasSecret: c.Secret != "", Events: c.Events, Rules: nz(c.Rules), Enabled: r.Enabled, CreatedBy: r.CreatedBy,
			GroupIDs: groupIDsOf(r.GroupID, ext), DutyUsers: c.DutyUsers}
		if e := emails[r.ID]; e != nil {
			v.Email, v.WebhookHint = e, e.HostHint()
		}
		if v.GroupIDs == nil {
			v.GroupIDs = []uint{}
		}
		if v.DutyUsers == nil {
			v.DutyUsers = []string{}
		}
		if l, ok := last[r.ID]; ok {
			sv := l.View()
			v.LastSend = &sv
		}
		out = append(out, v)
	}
	return out, nil
}

func nz(r []notify.Rule) []notify.Rule {
	if r == nil {
		return []notify.Rule{}
	}
	return r
}

// GetChannel 返回解密后的渠道（用于“测试发送”）。不存在返回 nil, nil。
func (s *Store) GetChannel(id uint) (*notify.Channel, error) {
	var row model.NotifyChannel
	err := s.DB.First(&row, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ext *model.NotifyChannelExt
	if s.NotifyExtReady() {
		var exts []model.NotifyChannelExt
		if err := s.DB.Where("channel_id = ?", id).Limit(1).Find(&exts).Error; err == nil && len(exts) > 0 {
			ext = &exts[0]
		}
	}
	c := s.toChannel(row, ext)
	if row.ChannelType == notify.TypeEmail {
		c.Email = s.channelEmails()[row.ID]
	}
	return &c, nil
}

// SaveChannel 创建（id=0）或修改渠道。URL 与密钥加密后入库；扩展表可用时同时写资源组列表、值班人员，并标记订阅按细分事件模型保存。
func (s *Store) SaveChannel(id uint, in notify.ChannelInput) (uint, error) {
	rules, _ := json.Marshal(nz(in.Rules))
	var row model.NotifyChannel
	if id != 0 {
		if err := s.DB.First(&row, id).Error; err != nil {
			return 0, err
		}
	} else {
		row.CreatedBy = in.Operator
	}
	groupID := in.GroupID
	if len(in.GroupIDs) > 0 {
		groupID = in.GroupIDs[0] // group_id 保持第一个资源组，旧代码按单个资源组读取仍然可用
	}
	row.GroupID, row.Name, row.ChannelType = groupID, in.Name, in.Type
	row.Events, row.Mentions, row.Enabled = strings.Join(in.Events, ","), string(rules), in.Enabled
	if in.URL != "" {
		row.WebhookURL = s.Mirage.Encrypt(in.URL)
	}
	if in.ClearSecret {
		row.Secret = ""
	} else if in.Secret != "" {
		row.Secret = s.Mirage.Encrypt(in.Secret)
	}
	extReady := s.NotifyExtReady()
	emailReady := s.NotifyEmailReady()
	var emailJSON []byte
	if in.Type == notify.TypeEmail {
		if !emailReady {
			return 0, errors.New("邮件渠道需要先在数据库里执行 deploy/sql/210_notify_email.sql")
		}
		if in.Email != nil {
			emailJSON, _ = json.Marshal(in.Email)
		} else if id == 0 {
			return 0, errors.New("邮件渠道缺少 SMTP 配置")
		}
		row.WebhookURL = "" // 邮件渠道没有 webhook 地址
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		if in.Type == notify.TypeEmail {
			if emailJSON != nil {
				if err := tx.Save(&model.NotifyEmail{ChannelID: row.ID, Config: string(emailJSON), UpdateTime: time.Now()}).Error; err != nil {
					return err
				}
			}
		} else if emailReady && id != 0 { // 渠道从邮件改成了其它类型：不再保留 SMTP 配置
			if err := tx.Where("channel_id = ?", id).Delete(&model.NotifyEmail{}).Error; err != nil {
				return err
			}
		}
		if !extReady {
			return nil
		}
		ext := model.NotifyChannelExt{ChannelID: row.ID, DutyUsers: strings.Join(in.DutyUsers, ","), EventsV2: true, UpdateTime: time.Now()}
		if len(in.GroupIDs) > 1 {
			ext.GroupIDs = joinIDs(in.GroupIDs)
		}
		return tx.Save(&ext).Error
	})
	if err != nil {
		return 0, err
	}
	return row.ID, nil
}

func (s *Store) DeleteChannel(id uint) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Delete(&model.NotifyChannel{}, id).Error; err != nil {
			return err
		}
		if s.NotifyEmailReady() {
			if err := tx.Where("channel_id = ?", id).Delete(&model.NotifyEmail{}).Error; err != nil {
				return err
			}
		}
		if s.NotifyExtReady() {
			return tx.Where("channel_id = ?", id).Delete(&model.NotifyChannelExt{}).Error
		}
		return nil
	})
}

// ListAuthGroups 返回全部权限组（@ 规则里选“某个组”用）。
func (s *Store) ListAuthGroups() ([]notify.IDName, error) {
	var rows []model.AuthGroup
	err := s.DB.Order("id").Find(&rows).Error
	out := make([]notify.IDName, 0, len(rows))
	for _, r := range rows {
		out = append(out, notify.IDName{ID: r.ID, Name: r.Name})
	}
	return out, err
}

// ListResourceGroups 返回全部未删除的资源组。
func (s *Store) ListResourceGroups() ([]notify.IDName, error) {
	var rows []model.ResourceGroup
	err := s.DB.Where("is_deleted = 0").Order("group_id").Find(&rows).Error
	out := make([]notify.IDName, 0, len(rows))
	for _, r := range rows {
		out = append(out, notify.IDName{ID: r.GroupID, Name: r.GroupName})
	}
	return out, err
}

// ---- 发送记录 ----

const (
	logKeepDays     = 30
	logCleanEvery   = time.Hour
	logErrorMaxRune = 500
)

var (
	logCleanMu   sync.Mutex
	logCleanedAt time.Time
)

// RecordSend 实现 notify.LogSink：记一条发送结果；表不存在或写入失败只记日志。写入时顺带（至多每小时一次）清理 30 天前的记录。
func (s *Store) RecordSend(rec notify.SendLog) {
	if !s.LogTableReady() {
		return
	}
	if r := []rune(rec.Error); len(r) > logErrorMaxRune {
		rec.Error = string(r[:logErrorMaxRune])
	}
	at := rec.At
	if at.IsZero() {
		at = time.Now()
	}
	row := model.NotifyLog{ChannelID: rec.ChannelID, ChannelName: cutRunes(rec.ChannelName, 50), Event: cutRunes(rec.Event, 32), Kind: cutRunes(rec.Kind, 32),
		OK: rec.OK, CostMs: rec.CostMs, Error: rec.Error, Mentions: rec.Mentions, IsTest: rec.Test, CreatedAt: at}
	db := s.DB.Session(&gorm.Session{Logger: s.DB.Logger.LogMode(logger.Silent)})
	if err := db.Create(&row).Error; err != nil {
		slog.Warn("记录通知发送结果失败", "err", err)
		return
	}
	logCleanMu.Lock()
	due := time.Since(logCleanedAt) >= logCleanEvery
	if due {
		logCleanedAt = time.Now()
	}
	logCleanMu.Unlock()
	if due {
		if err := db.Where("created_at < ?", time.Now().AddDate(0, 0, -logKeepDays)).Delete(&model.NotifyLog{}).Error; err != nil {
			slog.Warn("清理通知发送记录失败", "err", err)
		}
	}
}

func cutRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func logOf(r model.NotifyLog) notify.SendLog {
	return notify.SendLog{ID: r.ID, ChannelID: r.ChannelID, ChannelName: r.ChannelName, Event: r.Event, Kind: r.Kind, OK: r.OK, CostMs: r.CostMs,
		Error: r.Error, Mentions: r.Mentions, Test: r.IsTest, At: r.CreatedAt}
}

// LastSends 返回每个渠道最近一次发送（含测试发送）。
func (s *Store) LastSends() (map[uint]notify.SendLog, error) {
	out := map[uint]notify.SendLog{}
	if !s.LogTableReady() {
		return out, nil
	}
	var rows []model.NotifyLog
	err := s.DB.Raw("SELECT l.* FROM dbt_notify_log l JOIN (SELECT channel_id, MAX(id) AS mid FROM dbt_notify_log GROUP BY channel_id) m ON m.mid = l.id").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.ChannelID] = logOf(r)
	}
	return out, nil
}

// ListSendLogs 分页返回发送记录（新的在前）。
func (s *Store) ListSendLogs(f notify.LogFilter) ([]notify.SendLog, int64, error) {
	if !s.LogTableReady() {
		return []notify.SendLog{}, 0, nil
	}
	q := s.DB.Model(&model.NotifyLog{})
	if f.ChannelID != 0 {
		q = q.Where("channel_id = ?", f.ChannelID)
	}
	if f.OnlyFailed {
		q = q.Where("ok = 0")
	}
	if f.Event != "" {
		q = q.Where("event = ?", f.Event)
	}
	if f.Since != nil {
		q = q.Where("created_at >= ?", *f.Since)
	}
	q = q.Session(&gorm.Session{})
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []model.NotifyLog
	if err := q.Order("id DESC").Offset(f.Offset).Limit(f.Limit).Find(&rows).Error; err != nil {
		return nil, 0, err
	}
	out := make([]notify.SendLog, len(rows))
	for i, r := range rows {
		out[i] = logOf(r)
	}
	return out, total, nil
}

// SendStats 汇总 since 以来的发送次数与失败次数。
func (s *Store) SendStats(since time.Time) (notify.SendStats, error) {
	var st notify.SendStats
	if !s.LogTableReady() {
		return st, nil
	}
	var row struct{ Total, Failed int64 }
	err := s.DB.Raw("SELECT COUNT(*) AS total, COALESCE(SUM(CASE WHEN ok = 0 THEN 1 ELSE 0 END), 0) AS failed FROM dbt_notify_log WHERE created_at >= ?", since).Scan(&row).Error
	st.Total, st.Failed = row.Total, row.Failed
	return st, err
}

// SetChannelEnabled 实现 notify.EnabledSetter：只改启用状态。
func (s *Store) SetChannelEnabled(id uint, enabled bool) error {
	res := s.DB.Model(&model.NotifyChannel{}).Where("id = ?", id).Update("enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	return nil
}
