package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/migrations"
	"github.com/pocketbase/pocketbase/tools/types"
)

func init() {
	migrations.Register(func(app core.App) error {
		if _, err := app.FindCollectionByNameOrId("orders"); err == nil {
			return nil // already exists
		}

		orders := core.NewBaseCollection("orders")
		orders.ListRule = types.Pointer("")
		orders.ViewRule = types.Pointer("")
		orders.Fields.Add(
			&core.TextField{Name: "orderId", Required: true},
			&core.TextField{Name: "customerRef", Required: true},
			&core.TextField{Name: "status", Required: true},
			&core.NumberField{Name: "total"},
			&core.NumberField{Name: "lineCount"},
		)
		orders.Indexes = types.JSONArray[string]{
			"CREATE UNIQUE INDEX idx_orders_orderId ON orders (orderId)",
		}
		if err := app.Save(orders); err != nil {
			return err
		}

		lines := core.NewBaseCollection("order_lines")
		lines.ListRule = types.Pointer("")
		lines.ViewRule = types.Pointer("")
		lines.Fields.Add(
			&core.TextField{Name: "lineKey", Required: true},
			&core.RelationField{Name: "order", CollectionId: orders.Id, MaxSelect: 1, Required: true, CascadeDelete: true},
			&core.TextField{Name: "sku", Required: true},
			&core.NumberField{Name: "qty", Required: true},
			&core.NumberField{Name: "price", Required: true},
		)
		lines.Indexes = types.JSONArray[string]{
			"CREATE UNIQUE INDEX idx_order_lines_lineKey ON order_lines (lineKey)",
		}
		return app.Save(lines)
	}, func(app core.App) error {
		for _, name := range []string{"order_lines", "orders"} {
			c, err := app.FindCollectionByNameOrId(name)
			if err == nil {
				if err := app.Delete(c); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
