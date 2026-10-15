package casbin

import "slices"

func containsString(s []string, v string) bool {
	return slices.Contains(s, v)
}

func stringSliceToInterfaceSlice(s []string) []any {
	res := make([]any, len(s))
	for i, v := range s {
		res[i] = v
	}
	return res
}
