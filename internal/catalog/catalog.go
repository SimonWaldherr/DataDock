package catalog

import (
	"context"

	tinysql "github.com/SimonWaldherr/tinySQL"
)

type Object struct {
	Name string
	Type string
}

func ListObjects(ctx context.Context, db *tinysql.DB, tenant string) ([]Object, error) {
	_ = ctx
	var objects []Object
	for _, table := range db.ListTables(tenant) {
		if table != nil {
			objects = append(objects, Object{Name: table.Name, Type: "TABLE"})
		}
	}
	if db.Catalog() != nil {
		for _, view := range db.Catalog().GetViews() {
			if view != nil {
				objects = append(objects, Object{Name: view.Name, Type: "VIEW"})
			}
		}
	}
	return objects, nil
}
