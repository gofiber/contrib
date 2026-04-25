package casbin

func containsString(s []string, v string) bool {
	for _, vv := range s {
		if vv == v {
			return true
		}
	}
	return false
}

func stringSliceToInterfaceSlice(s []string) []interface{} {
	res := make([]interface{}, len(s))
	for i, v := range s {
		res[i] = v
	}
	return res
}
