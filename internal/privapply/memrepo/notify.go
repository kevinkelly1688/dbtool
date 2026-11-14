// @author: kevinkelly
// @license: Apache Licence
// @file: notify.go
// @time: 2026/10/28

package memrepo

import (
	"errors"
	"sort"
	"strings"

	"dbtool/internal/notify"
)

var (
	_ notify.Source    = (*Repo)(nil)
	_ notify.Directory = (*Repo)(nil)
	_ notify.Admin     = (*Repo)(nil)

	_ notify.EmailAdmin     = (*Repo)(nil)
	_ notify.GroupDirectory = (*Repo)(nil)
)

func hostOf(raw string) string {
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
	if i := strings.IndexAny(raw, "/?"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// Channels 实现 notify.Source：该资源组的 + 全局的启用渠道，以及旧 webhook 列（同类型没有新渠道时才生效）。
func (r *Repo) Channels(groupID uint) ([]notify.Channel, error) {
	var out []notify.Channel
	have := map[string]bool{}
	for _, c := range r.NotifyChannels {
		if c.Enabled && (c.GroupID == groupID || c.GroupID == 0) {
			out = append(out, c)
			have[c.Type] = true
		}
	}
	for _, g := range r.RGs {
		if g.GroupID != groupID {
			continue
		}
		for _, l := range []struct{ typ, url string }{{notify.TypeDingTalk, g.DingWebhook}, {notify.TypeFeishu, g.FeishuWebhook}, {notify.TypeSlack, g.SlackWebhook}} {
			if strings.TrimSpace(l.url) != "" && !have[l.typ] {
				out = append(out, notify.Channel{GroupID: groupID, Name: "旧版 webhook", Type: l.typ, WebhookURL: l.url, Enabled: true, Legacy: true})
			}
		}
	}
	return out, nil
}

func (r *Repo) person(id uint) notify.Person {
	u := r.Users[id]
	return notify.Person{Username: u.Username, Display: u.Display, DingID: u.DingUserID, FeishuID: u.FeishuOpenID, SlackID: u.SlackID, Email: strings.TrimSpace(u.Email)}
}

// ResourceGroupMembers 实现 notify.GroupDirectory：属于某资源组的启用用户。
func (r *Repo) ResourceGroupMembers(resourceGroupID uint) ([]notify.Person, error) {
	var ids []uint
	for uid, rgs := range r.UserRGs {
		for _, rid := range rgs {
			if rid == resourceGroupID && r.Users[uid].IsActive {
				ids = append(ids, uid)
				break
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []notify.Person
	for _, id := range ids {
		out = append(out, r.person(id))
	}
	return out, nil
}

// UsersByName 实现 notify.Directory。
func (r *Repo) UsersByName(names []string) ([]notify.Person, error) {
	var out []notify.Person
	for id, u := range r.Users {
		for _, n := range names {
			if u.Username == n {
				out = append(out, r.person(id))
			}
		}
	}
	return out, nil
}

// AuthGroupMembers 实现 notify.Directory。
func (r *Repo) AuthGroupMembers(authGroupID, resourceGroupID uint) ([]notify.Person, error) {
	var ids []uint
	for uid, gs := range r.Members {
		if !gs[authGroupID] {
			continue
		}
		for _, rid := range r.UserRGs[uid] {
			if rid == resourceGroupID {
				ids = append(ids, uid)
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out []notify.Person
	for _, id := range ids {
		out = append(out, r.person(id))
	}
	return out, nil
}

// ---- notify.Admin ----

func (r *Repo) NotifyTableReady() bool { return true }

// NotifyEmailReady 实现 notify.EmailAdmin：内存仓库总是支持邮件渠道。
func (r *Repo) NotifyEmailReady() bool { return true }

func (r *Repo) ListChannelViews() ([]notify.ChannelView, error) {
	out := make([]notify.ChannelView, 0, len(r.NotifyChannels))
	for _, c := range r.NotifyChannels {
		rules := c.Rules
		if rules == nil {
			rules = []notify.Rule{}
		}
		hint := hostOf(c.WebhookURL)
		if c.Email != nil {
			hint = c.Email.HostHint()
		}
		out = append(out, notify.ChannelView{ID: c.ID, GroupID: c.GroupID, Name: c.Name, Type: c.Type, WebhookHint: hint,
			HasSecret: c.Secret != "", Events: c.Events, Rules: rules, Enabled: c.Enabled, Email: c.Email})
	}
	return out, nil
}

func (r *Repo) GetChannel(id uint) (*notify.Channel, error) {
	for i := range r.NotifyChannels {
		if r.NotifyChannels[i].ID == id {
			c := r.NotifyChannels[i]
			return &c, nil
		}
	}
	return nil, nil
}

func (r *Repo) SaveChannel(id uint, in notify.ChannelInput) (uint, error) {
	if id == 0 {
		r.nextChannelID++
		r.NotifyChannels = append(r.NotifyChannels, notify.Channel{ID: r.nextChannelID})
		id = r.nextChannelID
	}
	for i := range r.NotifyChannels {
		if r.NotifyChannels[i].ID != id {
			continue
		}
		c := &r.NotifyChannels[i]
		c.GroupID, c.Name, c.Type, c.Events, c.Rules, c.Enabled = in.GroupID, in.Name, in.Type, in.Events, in.Rules, in.Enabled
		if in.URL != "" {
			c.WebhookURL = in.URL
		}
		if in.Type == notify.TypeEmail {
			c.WebhookURL = ""
			if in.Email != nil {
				cfg := *in.Email
				c.Email = &cfg
			}
		} else {
			c.Email = nil
		}
		if in.ClearSecret {
			c.Secret = ""
		} else if in.Secret != "" {
			c.Secret = in.Secret
		}
		return id, nil
	}
	return 0, errors.New("渠道不存在")
}

func (r *Repo) DeleteChannel(id uint) error {
	for i := range r.NotifyChannels {
		if r.NotifyChannels[i].ID == id {
			r.NotifyChannels = append(r.NotifyChannels[:i], r.NotifyChannels[i+1:]...)
			return nil
		}
	}
	return nil
}

func (r *Repo) ListAuthGroups() ([]notify.IDName, error) {
	var out []notify.IDName
	for id, g := range r.Groups {
		if g != nil {
			out = append(out, notify.IDName{ID: id, Name: g.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *Repo) ListResourceGroups() ([]notify.IDName, error) {
	var out []notify.IDName
	for _, g := range r.RGs {
		out = append(out, notify.IDName{ID: g.GroupID, Name: g.GroupName})
	}
	return out, nil
}
