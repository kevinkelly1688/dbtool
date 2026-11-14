// @author: kevinkelly
// @license: Apache Licence
// @file: errmap_test.go
// @time: 2026/10/28

package sysadmin

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	mysqldrv "github.com/go-sql-driver/mysql"
)

// 并发提交同名记录时唯一索引报 1062，应当返回 409 而不是 500（真库冒烟发现）。
func TestWriteErrDuplicateKeyIs409(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	writeErr(c, fmt.Errorf("insert: %w", &mysqldrv.MySQLError{Number: 1062, Message: "Duplicate entry 'x' for key 'name'"}))
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"conflict"`) {
		t.Fatalf("1062 应当是 409 conflict，得到 %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Duplicate entry") {
		t.Fatalf("不应把数据库错误原文返回给前端：%s", w.Body.String())
	}

	w = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", nil)
	writeErr(c, errors.New("boom"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("其他错误仍是 500，得到 %d", w.Code)
	}
}
