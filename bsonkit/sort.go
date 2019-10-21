package bsonkit

import "sort"

type Column struct {
	Path    string
	Reverse bool
}

func Sort(list List, columns []Column) {
	// sort slice by comparing values
	sort.Slice(list, func(i, j int) bool {
		return Order(list[i], list[j], columns) < 0
	})
}

func Order(l, r Doc, columns []Column) int {
	for _, column := range columns {
		// get values
		a := Get(l, column.Path)
		b := Get(r, column.Path)

		// compare values
		res := Compare(a, b)

		// continue if equal
		if res == 0 {
			continue
		}

		// check if reverse
		if column.Reverse {
			return res * -1
		}

		return res
	}

	return 0
}
