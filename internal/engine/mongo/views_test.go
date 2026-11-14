// @author: kevinkelly
// @license: Apache Licence
// @file: views_test.go
// @time: 2026/10/28

package mongo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func stage(k string, v any) bson.D { return bson.D{{Key: k, Value: v}} }

func viewFixture() (*Engine, *fakeBackend, *fakeDB) {
	be := newFake()
	d := be.db("app")
	d.coll("users")
	d.coll("orders")
	d.infos = append(d.infos,
		CollectionInfo{Name: "v1", Type: "view", Options: bson.D{{Key: "viewOn", Value: "users"}, {Key: "pipeline", Value: bson.A{
			stage("$project", bson.D{{Key: "p", Value: "$profile.phone"}, {Key: "name", Value: int32(1)}}),
			stage("$lookup", bson.D{{Key: "from", Value: "orders"}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "uid"}, {Key: "as", Value: "o"}}),
		}}}},
		CollectionInfo{Name: "v2", Type: "view", Options: bson.D{{Key: "viewOn", Value: "v1"}, {Key: "pipeline", Value: bson.A{}}}},
		CollectionInfo{Name: "bad", Type: "view", Options: bson.D{{Key: "pipeline", Value: bson.A{}}}},
		CollectionInfo{Name: "badpipe", Type: "view", Options: bson.D{{Key: "viewOn", Value: "users"}, {Key: "pipeline", Value: bson.A{int32(1)}}}},
	)
	return NewWithBackend(be), be, d
}

func TestViewInfos(t *testing.T) {
	e, _, _ := viewFixture()
	got, err := e.ViewInfos(context.Background(), "app", []string{"users", "v1", "v2", "bad", "badpipe", "missing", "$bad", "system.views", ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("普通集合、不存在的、不合法的名字都不应返回: %+v", got)
	}
	by := map[string]ViewInfo{}
	for _, v := range got {
		by[v.Name] = v
	}
	v1 := by["v1"]
	if v1.ViewOn != "users" || v1.Err != "" || len(v1.Pipeline) != 2 || v1.Pipeline[0][0].Key != "$project" {
		t.Errorf("v1=%+v", v1)
	}
	if _, ok := v1.Pipeline[0][0].Value.(bson.D); !ok {
		t.Errorf("管道阶段里的文档应是 bson.D: %T", v1.Pipeline[0][0].Value)
	}
	if v2 := by["v2"]; v2.ViewOn != "v1" || v2.Err != "" || len(v2.Pipeline) != 0 {
		t.Errorf("v2=%+v", v2)
	}
	if b := by["bad"]; b.Err == "" || b.ViewOn != "" {
		t.Errorf("没有 viewOn 的视图读不到定义: %+v", b)
	}
	if b := by["badpipe"]; b.Err == "" {
		t.Errorf("管道里有不是文档的阶段: %+v", b)
	}
	// 结果按名字排序
	var names []string
	for _, v := range got {
		names = append(names, v.Name)
	}
	if !reflect.DeepEqual(names, []string{"bad", "badpipe", "v1", "v2"}) {
		t.Errorf("names=%v", names)
	}
	// 没有可查的名字：不调用 listCollections
	if got, err := e.ViewInfos(context.Background(), "app", []string{"", "$x"}); err != nil || got != nil {
		t.Errorf("空名单: %v %v", got, err)
	}
	// 系统库、不合法的库名
	if _, err := e.ViewInfos(context.Background(), "admin", []string{"v1"}); err == nil {
		t.Error("系统库应拒绝")
	}
}

func TestViewInfosListError(t *testing.T) {
	e, _, d := viewFixture()
	d.err = errors.New("not authorized on app to execute command { listCollections }")
	if _, err := e.ViewInfos(context.Background(), "app", []string{"v1"}); err == nil {
		t.Fatal("listCollections 出错应上抛，不能当作没有视图")
	}
}

// 库句柄支持按名字过滤时只带要查的名字，不必把整个库的集合列一遍。
type namedDB struct {
	*fakeDB
	asked *[][]string
}

func (d namedDB) ListCollectionsByName(_ context.Context, names []string) ([]CollectionInfo, error) {
	*d.asked = append(*d.asked, names)
	var out []CollectionInfo
	for _, ci := range d.fakeDB.infos {
		for _, n := range names {
			if ci.Name == n {
				out = append(out, ci)
			}
		}
	}
	return out, nil
}

type namedBackend struct {
	*fakeBackend
	asked [][]string
}

func (b *namedBackend) Database(name string) Database {
	return namedDB{b.fakeBackend.db(name), &b.asked}
}

func TestViewInfosFiltersByName(t *testing.T) {
	_, be, _ := viewFixture()
	nb := &namedBackend{fakeBackend: be}
	e := NewWithBackend(nb)
	got, err := e.ViewInfos(context.Background(), "app", []string{"v2", "users", "v1", "v1"})
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if want := [][]string{{"users", "v1", "v2"}}; !reflect.DeepEqual(nb.asked, want) {
		t.Errorf("应只带去重、排序后的名字: %v", nb.asked)
	}
}

func TestPipelineCollections(t *testing.T) {
	p := []bson.D{
		stage("$match", bson.D{{Key: "a", Value: int32(1)}}),
		stage("$lookup", bson.D{{Key: "from", Value: "orders"}, {Key: "as", Value: "o"}}),
		stage("$lookup", bson.D{{Key: "from", Value: "addr"}, {Key: "pipeline", Value: bson.A{
			stage("$lookup", bson.D{{Key: "from", Value: "geo"}}),
		}}}),
		stage("$graphLookup", bson.D{{Key: "from", Value: "tree"}}),
		stage("$unionWith", "archive"),
		stage("$unionWith", bson.D{{Key: "coll", Value: "old"}, {Key: "pipeline", Value: bson.A{stage("$unionWith", "older")}}}),
		stage("$facet", bson.D{{Key: "f1", Value: bson.A{stage("$lookup", bson.D{{Key: "from", Value: "facetc"}})}}, {Key: "f2", Value: bson.A{}}}),
	}
	want := []string{"addr", "archive", "facetc", "geo", "old", "older", "orders", "tree"}
	if got := PipelineCollections(p); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	if got := PipelineCollections(nil); len(got) != 0 {
		t.Errorf("空管道: %v", got)
	}
	// 嵌套太深不会无限递归
	deep := []bson.D{stage("$lookup", bson.D{{Key: "from", Value: "x"}})}
	for i := 0; i < 20; i++ {
		deep = []bson.D{stage("$lookup", bson.D{{Key: "from", Value: "c"}, {Key: "pipeline", Value: bson.A{deep[0]}}})}
	}
	if got := PipelineCollections(deep); len(got) == 0 || !strings.Contains(strings.Join(got, ","), "c") {
		t.Errorf("deep=%v", got)
	}
}

func TestPipelineOfNormalizesDocuments(t *testing.T) {
	// 驱动按 bson.M 解出来的阶段（key 顺序不定）也要转成 bson.D，沿管道追踪改名时只认 bson.D
	p, why := pipelineOf(bson.A{bson.M{"$project": bson.M{"p": "$profile.phone"}}, map[string]any{"$set": map[string]any{"q": "$p"}}})
	if why != "" || len(p) != 2 {
		t.Fatalf("%v %q", p, why)
	}
	if d, ok := p[0][0].Value.(bson.D); !ok || d[0].Key != "p" {
		t.Errorf("嵌套文档应转成 bson.D: %#v", p[0])
	}
	if d, ok := p[1][0].Value.(bson.D); !ok || d[0].Key != "q" {
		t.Errorf("map 也要转: %#v", p[1])
	}
	if p, why := pipelineOf(nil); p != nil || why != "" {
		t.Errorf("没有 pipeline 不算错: %v %q", p, why)
	}
	if _, why := pipelineOf("x"); why == "" {
		t.Error("pipeline 不是数组应报")
	}
}
