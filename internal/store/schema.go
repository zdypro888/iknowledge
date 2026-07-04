package store

import (
	"reflect"
	"strings"
	"time"
)

// mergeSchema 是未知字段合并用的"已知字段树",由结构体 yaml tag 反射派生:
// known 是本层已知字段名;children 是字段名 → 子层 schema(struct/slice-of-struct)。
// 模型加字段(schema 演进"加字段不升号")时自动跟进,不需手工维护清单。
type mergeSchema struct {
	known    map[string]bool
	children map[string]*mergeSchema
}

func (s *mergeSchema) child(key string) *mergeSchema {
	if s == nil {
		return nil
	}
	return s.children[key]
}

var timeType = reflect.TypeFor[time.Time]()

// buildSchema 从结构体类型派生 mergeSchema。
func buildSchema(t reflect.Type) *mergeSchema {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == timeType {
		return nil
	}
	sc := &mergeSchema{known: map[string]bool{}, children: map[string]*mergeSchema{}}
	for f := range t.Fields() {
		if !f.IsExported() {
			continue
		}
		name := yamlName(f)
		if name == "-" {
			continue
		}
		sc.known[name] = true
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
			ft = ft.Elem()
		}
		if child := buildSchema(ft); child != nil {
			sc.children[name] = child
		}
	}
	return sc
}

func yamlName(f reflect.StructField) string {
	tag := f.Tag.Get("yaml")
	if tag == "" {
		return strings.ToLower(f.Name)
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return strings.ToLower(f.Name)
	}
	return name
}

// shardSchema 是分片文件的已知字段树(Shard → Node → Anchor/Entry)。
var shardSchema = buildSchema(reflect.TypeFor[Shard]())
