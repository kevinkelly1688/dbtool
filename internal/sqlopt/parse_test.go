// @author: kevinkelly
// @license: Apache Licence
// @file: parse_test.go
// @time: 2026/10/28

package sqlopt

import (
	"strings"
	"testing"
)

const mapperXML = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE mapper PUBLIC "-//mybatis.org//DTD Mapper 3.0//EN" "http://mybatis.org/dtd/mybatis-3-mapper.dtd">
<mapper namespace="com.shop.OrderMapper">
  <sql id="cols">id, user_id, ${alias}.amount</sql>
  <select id="listByUser" resultType="Order">
    select <include refid="cols"><property name="alias" value="o"/></include> from orders o
    <where>
      <if test="userId != null">and o.user_id = #{userId}</if>
      <if test="status != null">or o.status = #{status,jdbcType=VARCHAR}</if>
    </where>
    order by created_at desc
  </select>
  <update id="decStock">
    update stock
    <set>
      qty = qty - #{n},
      <if test="x">note = #{note},</if>
    </set>
    where sku = #{sku}
  </update>
  <delete id="clean"><![CDATA[ delete from log_tmp where created_at < #{d} ]]></delete>
  <select id="byIds">
    select * from t where id in
    <foreach collection="ids" item="i" open="(" separator="," close=")">#{i}</foreach>
  </select>
  <select id="dyn">
    select * from ${table} where 1=1
    <choose><when test="a">and a = #{a}</when><otherwise>and b = #{b}</otherwise></choose>
  </select>
  <insert id="listByUser">insert into t (a) values (#{a})</insert>
</mapper>`

func TestParseMapper(t *testing.T) {
	rows, format, err := ParseText(mapperXML)
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatMapper {
		t.Fatalf("format=%s", format)
	}
	got := map[string]string{}
	var ids []string
	for _, r := range rows {
		got[r.ID] = r.SQL
		ids = append(ids, r.ID)
	}
	want := map[string]string{
		"listByUser":   "select id, user_id, o.amount from orders o WHERE o.user_id = ? or o.status = ? order by created_at desc",
		"decStock":     "update stock SET qty = qty - ?, note = ? where sku = ?",
		"clean":        "delete from log_tmp where created_at < ?",
		"byIds":        "select * from t where id in (?)",
		"dyn":          "select * from table where 1=1 and a = ?",
		"listByUser#2": "insert into t (a) values (?)",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s:\n got %q\nwant %q", k, got[k], w)
		}
	}
	if len(rows) != len(want) {
		t.Errorf("语句数 %d，ids=%v", len(rows), ids)
	}
}

func TestParseMapperRobust(t *testing.T) {
	// 带 DOCTYPE 内部实体：不展开、不报告资源错误
	xxe := `<?xml version="1.0"?><!DOCTYPE mapper [<!ENTITY x SYSTEM "file:///etc/passwd">]><mapper><select id="a">select '&x;' from t</select></mapper>`
	rows, _, err := ParseText(xxe)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.Contains(r.SQL, "root:") {
			t.Fatal("不应读取外部实体")
		}
	}
	// 不是合法 XML：按普通 SQL 文本处理，不报错
	rows, format, err := ParseText("<mapper><select id=\"a\">select 1 from")
	if err != nil || format != FormatSQL || len(rows) == 0 {
		t.Fatalf("%v %s %+v", err, format, rows)
	}
	// <include> 套娃放大：必须被限制住，不会耗尽内存
	var sb strings.Builder
	sb.WriteString(`<mapper><sql id="s0">select 1</sql>`)
	for i := 1; i <= 20; i++ {
		sb.WriteString(`<sql id="s` + itoa(i) + `"><include refid="s` + itoa(i-1) + `"/><include refid="s` + itoa(i-1) + `"/></sql>`)
	}
	sb.WriteString(`<select id="boom"><include refid="s20"/></select></mapper>`)
	rows, _, err = ParseText(sb.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Err == "" {
		t.Fatalf("套娃展开应被标记为过大：%+v", rows)
	}
	// 嵌套过深
	deep := "<mapper><select id=\"d\">" + strings.Repeat("<if>", 200) + "x" + strings.Repeat("</if>", 200) + "</select></mapper>"
	if _, _, err := ParseText(deep); err != nil {
		t.Fatalf("过深嵌套按普通文本处理，不应报错：%v", err)
	}
}

const slowLog = `/usr/sbin/mysqld, Version: 8.0.36 (MySQL Community Server). started with:
Tcp port: 3306  Unix socket: /var/run/mysqld/mysqld.sock
Time                 Id Command    Argument
# Time: 2026-10-05T09:02:11.123456Z
# User@Host: app_rw[app_rw] @  [10.20.1.8]  Id:   123
# Query_time: 4.200000  Lock_time: 0.000100 Rows_sent: 5000  Rows_examined: 9821004
use shop;
SET timestamp=1790000531;
select * from orders where status='paid' order by created_at desc limit 20;
# Time: 2026-10-05T09:02:15.000000Z
# User@Host: app_ro[app_ro] @ web01 [10.20.1.9]  Id:   124
# Query_time: 1.500000  Lock_time: 0.000000 Rows_sent: 1  Rows_examined: 300
SET timestamp=1790000535;
update stock set qty = qty - 1
  where sku = 'a;b';
# Time: 2026-10-05T09:03:00.000000Z
# User@Host: root[root] @ localhost []  Id: 5
# Query_time: 0.000010  Lock_time: 0.000000 Rows_sent: 0  Rows_examined: 0
# administrator command: Quit;
`

func TestParseSlowLog(t *testing.T) {
	rows, format, err := ParseText(slowLog)
	if err != nil {
		t.Fatal(err)
	}
	if format != FormatSlow {
		t.Fatalf("format=%s", format)
	}
	if len(rows) != 2 {
		t.Fatalf("应解析出 2 条（administrator command 没有 SQL）：%+v", rows)
	}
	a, b := rows[0], rows[1]
	if a.Stats == nil || a.Stats.QueryTime != 4.2 || a.Stats.RowsExamined != 9821004 || a.Stats.RowsSent != 5000 || a.Stats.DB != "shop" || a.Stats.User != "app_rw" || a.Stats.Host != "" && a.Stats.Host != "10.20.1.8" {
		t.Errorf("第一条统计：%+v", a.Stats)
	}
	if strings.HasPrefix(strings.ToLower(a.SQL), "set") || strings.Contains(a.SQL, "use shop") {
		t.Errorf("SET timestamp / use 应被剔除：%q", a.SQL)
	}
	if !strings.HasPrefix(a.SQL, "select * from orders") {
		t.Errorf("SQL=%q", a.SQL)
	}
	if b.Stats.DB != "shop" || b.Stats.Host != "web01" || !strings.Contains(b.SQL, "sku = 'a;b'") {
		t.Errorf("第二条：%+v %q", b.Stats, b.SQL)
	}
}

func TestParsePlainAndLimits(t *testing.T) {
	rows, format, err := ParseText("-- 注释\nselect 1;\n/* x */ select 'a;b' from t;;\n")
	if err != nil || format != FormatSQL || len(rows) != 2 || rows[1].SQL != "select 'a;b' from t" {
		t.Fatalf("%v %s %+v", err, format, rows)
	}
	if rows, _, err := ParseText("   \n"); err != nil || len(rows) != 0 {
		t.Fatalf("空内容：%v %+v", err, rows)
	}
	if _, _, err := ParseText("select \x00 1"); err == nil || codeOf(t, err) != "bad_request" {
		t.Fatalf("含 NUL 的内容应拒绝：%v", err)
	}
	if _, _, err := ParseText("select '\xff\xfe'"); err == nil {
		t.Fatal("非 UTF-8 的内容应拒绝")
	}
	if _, _, err := ParseText(strings.Repeat("a", MaxTextBytes+1)); err == nil {
		t.Fatal("超过大小上限应拒绝")
	}
	if rows, _, err := ParseText(utf8BOM + "select 1"); err != nil || len(rows) != 1 || rows[0].SQL != "select 1" {
		t.Fatalf("应忽略 BOM：%v %+v", err, rows)
	}
	many := strings.Repeat("select 1;", MaxStatements+1)
	if _, _, err := ParseText(many); err == nil {
		t.Fatal("语句数超过上限应拒绝")
	}
	long := "select '" + strings.Repeat("x", MaxSQLBytes) + "'"
	rows, _, err = ParseText(long + "; select 1")
	if err != nil || len(rows) != 2 || rows[0].Err == "" || rows[1].Err != "" {
		t.Fatalf("超长 SQL 应标记跳过：%v %+v", err, rows[0].Err)
	}
}
