// @author: kevinkelly
// @license: Apache Licence
// @file: reencrypt_test.go
// @time: 2026/10/28

package dbtadmin

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"dbtool/internal/config"
	"dbtool/internal/crypto"
)

func (m *memStore) EncryptedCells(context.Context) ([]Cell, error) {
	if m.cellsErr != nil {
		return nil, m.cellsErr
	}
	return append([]Cell(nil), m.cells...), nil
}

func (t *memTx) ReplaceCell(table, column, id, oldValue, newValue string) (bool, error) {
	t.m.setCalls++
	if t.m.failOnSet > 0 && t.m.setCalls == t.m.failOnSet {
		return false, errors.New("boom")
	}
	if t.m.staleAt > 0 && t.m.setCalls == t.m.staleAt {
		return false, nil
	}
	for i := range t.m.cells {
		c := &t.m.cells[i]
		if c.Table == table && c.Column == column && c.ID == id {
			if c.Value != oldValue {
				return false, nil
			}
			c.Value = newValue
			return true, nil
		}
	}
	return false, nil
}

// 一套“原样复制过来”的库：实例 / 配置是公开缺省密钥的密文，混了一条新密钥保存的、一条明文、一条谁都解不开的。
type rekeyFixture struct {
	st      *memStore
	oldM    *crypto.Mirage // 公开缺省密钥
	newM    *crypto.Mirage // dbtool 当前密钥
	otherM  *crypto.Mirage // 第三把
	alien   string         // 谁都解不开的密文
	alienPT string
}

func newRekeyFixture(t *testing.T) *rekeyFixture {
	t.Helper()
	f := &rekeyFixture{
		st:     newMemStore(),
		oldM:   mustMirage(t, config.DefaultSecretKey()),
		newM:   mustMirage(t, testNewKey),
		otherM: mustMirage(t, testOldKey),
	}
	f.alienPT = "TOP-SECRET-ALIEN-PASSWORD"
	f.alien = f.otherM.Encrypt(f.alienPT)
	f.st.cells = []Cell{
		{Table: "dbt_sql_instance", Column: "user", ID: "1", Ref: "prod-mysql-01", Value: f.oldM.Encrypt("dba_user_xyz"), MaxLen: 200},
		{Table: "dbt_sql_instance", Column: "password", ID: "1", Ref: "prod-mysql-01", Value: f.oldM.Encrypt("P@ssw0rd-very-secret"), MaxLen: 300},
		{Table: "dbt_sql_instance", Column: "password", ID: "2", Ref: "prod-mysql-02", Value: f.newM.Encrypt("saved-with-new-key"), MaxLen: 300},
		{Table: "dbt_sql_instance", Column: "password", ID: "3", Ref: "legacy-alien", Value: f.alien, MaxLen: 300},
		{Table: "dbt_ssh_tunnel", Column: "pkey_password", ID: "10", Ref: "storage_type", Value: f.oldM.Encrypt("local"), MaxLen: 500},
		{Table: "dbt_ssh_tunnel", Column: "pkey_password", ID: "11", Ref: "data_masking", Value: "true", MaxLen: 500},
		{Table: "dbt_ssh_tunnel", Column: "pkey_password", ID: "12", Ref: "long_plain", Value: strings.Repeat("x", 450), MaxLen: 500},
		{Table: "dbt_2fa_config", Column: "secret_key", ID: "5", Ref: "id=5", Value: f.oldM.Encrypt("JBSWY3DPEHPK3PXP"), MaxLen: 256},
	}
	f.st.tables[tblAuditLog] = true
	return f
}

func (f *rekeyFixture) env(t *testing.T) *testEnv {
	t.Helper()
	te := newTestEnv(t, f.st)
	te.env["SECRET_KEY"] = testNewKey
	te.env["MIRAGE_SECRET_KEY"] = testNewKey
	return te
}

func (f *rekeyFixture) cell(table, col, id string) string {
	for _, c := range f.st.cells {
		if c.Table == table && c.Column == col && c.ID == id {
			return c.Value
		}
	}
	return ""
}

func (f *rekeyFixture) secrets() []string {
	return []string{testNewKey, config.DefaultSecretKey(), testOldKey, "dba_user_xyz", "P@ssw0rd-very-secret", "saved-with-new-key", f.alienPT,
		f.alien, f.oldM.Encrypt("dba_user_xyz"), f.oldM.Encrypt("P@ssw0rd-very-secret"), f.oldM.Encrypt("local"), f.newM.Encrypt("saved-with-new-key"),
		"JBSWY3DPEHPK3PXP", f.oldM.Encrypt("JBSWY3DPEHPK3PXP")}
}

// 默认只演练：给出统计和名字，不写任何东西，输出里没有密钥 / 密文 / 明文。
func TestReencryptDryRunWritesNothing(t *testing.T) {
	f := newRekeyFixture(t)
	before := append([]Cell(nil), f.st.cells...)
	te := f.env(t)
	if code := te.run("reencrypt"); code != ExitOK {
		t.Fatalf("演练应返回 0: %d\n%s", code, te.all())
	}
	out := te.all()
	mustContain(t, "输出", out, "只演练", "没有写任何东西", "将改写", "legacy-alien", "解不开", "公开缺省密钥", "mysqldump")
	mustNotContain(t, "输出", out, f.secrets()...)
	for i, c := range f.st.cells {
		if c.Value != before[i].Value {
			t.Fatalf("演练不应改动任何值: %s.%s id=%s", c.Table, c.Column, c.ID)
		}
	}
	if f.st.txCount != 0 {
		t.Error("演练不应开事务")
	}
	if len(f.st.audits) != 0 {
		t.Error("演练不应写审计")
	}
}

// --apply --yes：旧密钥的密文换成当前密钥、明文被加密；已是当前密钥的、解不开的原样；超列宽的保留明文；写一条审计且不含值。
func TestReencryptApply(t *testing.T) {
	f := newRekeyFixture(t)
	te := f.env(t)
	if code := te.run("reencrypt", "--apply", "--yes"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	out := te.all()
	mustContain(t, "输出", out, "已改写 5 个值", "复查：仍可重加密 0")
	mustNotContain(t, "输出", out, f.secrets()...)

	if got := f.newM.Decrypt(f.cell("dbt_sql_instance", "user", "1")); got != "dba_user_xyz" {
		t.Errorf("user 应换成当前密钥: %q", got)
	}
	if got := f.newM.Decrypt(f.cell("dbt_sql_instance", "password", "1")); got != "P@ssw0rd-very-secret" {
		t.Errorf("password 应换成当前密钥: %q", got)
	}
	if got := f.cell("dbt_sql_instance", "password", "2"); got != f.newM.Encrypt("saved-with-new-key") {
		t.Error("已是当前密钥的不应改动")
	}
	if got := f.cell("dbt_sql_instance", "password", "3"); got != f.alien {
		t.Error("谁都解不开的应原样保留")
	}
	if got := f.cell("dbt_ssh_tunnel", "pkey_password", "11"); got == "true" || f.newM.Decrypt(got) != "true" {
		t.Errorf("明文应被当前密钥加密: %q", got)
	}
	if got := f.cell("dbt_ssh_tunnel", "pkey_password", "12"); got != strings.Repeat("x", 450) {
		t.Error("超列宽的明文应保留明文")
	}
	if len(f.st.audits) != 1 || f.st.audits[0].Action != "密文重加密" {
		t.Fatalf("应写一条审计: %+v", f.st.audits)
	}
	mustNotContain(t, "审计", f.st.audits[0].ExtraInfo, f.secrets()...)
	mustContain(t, "审计", f.st.audits[0].ExtraInfo, "改写 5 个加密值")

	// 重复运行：没有需要改的了
	te.reset()
	if code := te.run("reencrypt", "--apply", "--yes"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	mustContain(t, "第二次输出", te.all(), "没有需要改的值")
	if len(f.st.audits) != 1 {
		t.Error("没有改动时不应再写审计")
	}
}

// 没有 --yes 又不是终端：拒绝写入，一个值都不动。
func TestReencryptNeedsConfirmation(t *testing.T) {
	f := newRekeyFixture(t)
	before := append([]Cell(nil), f.st.cells...)
	te := f.env(t)
	te.tty = false
	if code := te.run("reencrypt", "--apply"); code != ExitUsage {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	for i, c := range f.st.cells {
		if c.Value != before[i].Value {
			t.Fatal("未确认不应改动")
		}
	}
	// 终端里输入 yes 才写
	te2 := f.env(t)
	te2.tty = true
	te2.stdin.WriteString("yes\n")
	if code := te2.run("reencrypt", "--apply"); code != ExitOK {
		t.Fatalf("终端确认后应写入: %d\n%s", code, te2.all())
	}
	if f.newM.Decrypt(f.cell("dbt_sql_instance", "user", "1")) != "dba_user_xyz" {
		t.Error("确认后应写入")
	}
}

// 事务里任何一步出错都整体回滚：一个值都不能留下改动。
func TestReencryptRollsBackOnError(t *testing.T) {
	f := newRekeyFixture(t)
	before := append([]Cell(nil), f.st.cells...)
	f.st.failOnSet = 3
	te := f.env(t)
	if code := te.run("reencrypt", "--apply", "--yes"); code != ExitFailed {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	mustContain(t, "输出", te.all(), "整体回滚")
	for i, c := range f.st.cells {
		if c.Value != before[i].Value {
			t.Fatalf("回滚后不应有改动: %s.%s id=%s", c.Table, c.Column, c.ID)
		}
	}
	if len(f.st.audits) != 0 {
		t.Error("回滚后不应有审计")
	}
}

// 读取之后值被别人改过：不覆盖，提示再运行。
func TestReencryptSkipsConcurrentlyChangedValue(t *testing.T) {
	f := newRekeyFixture(t)
	f.st.staleAt = 1
	te := f.env(t)
	if code := te.run("reencrypt", "--apply", "--yes"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	mustContain(t, "输出", te.all(), "已改写 4 个值", "被别人改过")
}

// 之前的部署单独配置过密钥：用 OLD_* 给出；SECRET_KEY / 公开缺省都解不开的值靠它换。
func TestReencryptWithExplicitOldKey(t *testing.T) {
	f := newRekeyFixture(t)
	te := f.env(t)
	te.env["OLD_MIRAGE_SECRET_KEY"] = testOldKey
	if code := te.run("reencrypt", "--apply", "--yes"); code != ExitOK {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	mustContain(t, "输出", te.all(), "OLD_* 显式给出的旧密钥")
	if got := f.newM.Decrypt(f.cell("dbt_sql_instance", "password", "3")); got != f.alienPT {
		t.Errorf("显式旧密钥应能解开原来解不开的值: %q", got)
	}
	mustNotContain(t, "输出", te.all(), f.secrets()...)
}

// 当前生效的密钥就是公开缺省值：演练照常给出统计并警告；写入一律拒绝（不能把数据加密到公开密钥上）。
func TestReencryptRefusesPublicDefaultKey(t *testing.T) {
	f := newRekeyFixture(t)
	before := append([]Cell(nil), f.st.cells...)
	te := newTestEnv(t, f.st)
	te.env["SECRET_KEY"] = config.DefaultSecretKey()
	if code := te.run("reencrypt"); code != ExitOK {
		t.Fatalf("演练应返回 0: %d\n%s", code, te.all())
	}
	mustContain(t, "演练输出", te.all(), "公开的缺省值", "只会把明文值按当前密钥加密")
	te.reset()
	if code := te.run("reencrypt", "--apply", "--yes"); code != ExitUsage {
		t.Fatalf("写入应被拒绝: %d\n%s", code, te.all())
	}
	mustContain(t, "输出", te.all(), "拒绝把数据加密到这把密钥上")
	for i, c := range f.st.cells {
		if c.Value != before[i].Value {
			t.Fatal("被拒绝时不应改动任何值")
		}
	}
}

// 当前生效的密钥无效（太短）时，不能退而求其次拿别的密钥当新密钥写库。
func TestReencryptRefusesInvalidCurrentKey(t *testing.T) {
	f := newRekeyFixture(t)
	before := append([]Cell(nil), f.st.cells...)
	te := newTestEnv(t, f.st)
	te.env["MIRAGE_SECRET_KEY"] = "short"
	te.env["SECRET_KEY"] = testNewKey
	if code := te.run("reencrypt", "--apply", "--yes"); code != ExitUsage {
		t.Fatalf("code=%d\n%s", code, te.all())
	}
	for i, c := range f.st.cells {
		if c.Value != before[i].Value {
			t.Fatal("不应改动任何值")
		}
	}
}

func TestReencryptUsageErrors(t *testing.T) {
	f := newRekeyFixture(t)
	te := newTestEnv(t, f.st)
	te.env["SECRET_KEY"] = "short"
	if code := te.run("reencrypt"); code != ExitUsage {
		t.Fatalf("密钥太短应是参数错误: %d\n%s", code, te.all())
	}
	te = f.env(t)
	te.env["OLD_MIRAGE_SECRET_KEY"] = "short"
	if code := te.run("reencrypt"); code != ExitUsage {
		t.Fatalf("OLD_ 密钥太短应是参数错误: %d\n%s", code, te.all())
	}
	te = f.env(t)
	delete(te.env, "DATABASE_URL")
	if code := te.run("reencrypt"); code != ExitUsage {
		t.Fatalf("缺 DATABASE_URL: %d", code)
	}
}

// 真实 mysqlStore：读出各表加密列 + 列宽；ReplaceCell 条件更新按字节比较，表 / 列只接受登记过的。
func TestMySQLStoreEncryptedCellsAndReplace(t *testing.T) {
	st, f := newFakeStore(t,
		tablesRule("dbt_ssh_tunnel"),
		func(q string, a []driver.Value) *fakeResult {
			if strings.Contains(q, "information_schema.columns") && strings.Contains(q, "character_maximum_length") {
				return &fakeResult{cols: []string{"column_name", "character_maximum_length"}, rows: [][]driver.Value{
					{"id", nil}, {"tunnel_name", int64(100)}, {"pkey_password", int64(500)}, {"description", int64(200)}}}
			}
			if strings.HasPrefix(q, "SELECT id, `tunnel_name`, `pkey_password` FROM `dbt_ssh_tunnel`") {
				return &fakeResult{cols: []string{"id", "item", "value"}, rows: [][]driver.Value{
					{int64(1), "storage_type", "CIPHERTEXT=="}, {int64(2), "empty_one", ""}, {int64(3), "null_one", nil}}}
			}
			return nil
		})
	cells, err := st.EncryptedCells(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 1 || cells[0].Table != "dbt_ssh_tunnel" || cells[0].Column != "pkey_password" || cells[0].ID != "1" ||
		cells[0].Ref != "storage_type" || cells[0].MaxLen != 500 || cells[0].Value != "CIPHERTEXT==" {
		t.Fatalf("cells: %+v", cells)
	}
	err = st.InTx(context.Background(), func(tx Tx) error {
		ok, err := tx.ReplaceCell("dbt_ssh_tunnel", "pkey_password", "1", "OLD==", "NEW==")
		if err != nil || !ok {
			t.Errorf("ReplaceCell: %v %v", ok, err)
		}
		if _, err := tx.ReplaceCell("dbt_sql_users", "password", "1", "a", "b"); err == nil {
			t.Error("没登记的表应被拒绝")
		}
		if _, err := tx.ReplaceCell("dbt_ssh_tunnel", "tunnel_name", "1", "a", "b"); err == nil {
			t.Error("没登记的列应被拒绝")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	qs := f.queries("UPDATE `dbt_ssh_tunnel`")
	if len(qs) != 1 || !strings.Contains(qs[0].sql, "BINARY `pkey_password` = BINARY ?") || len(qs[0].args) != 3 {
		t.Fatalf("UPDATE 语句不对: %+v", qs)
	}
	if strings.Contains(qs[0].sql, "NEW==") || strings.Contains(qs[0].sql, "OLD==") {
		t.Error("值必须走参数")
	}
}
