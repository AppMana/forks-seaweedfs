package weed_server

func filerSessionReaderBudget(wholeMB, sessionMB int64) int64 {
	budget := int64(64 << 20)
	if wholeMB > 0 {
		budget = min(max((wholeMB<<20)/4, int64(32<<20)), wholeMB<<20)
	}
	if sessionMB > 0 {
		budget = sessionMB << 20
	}
	if wholeMB > 0 {
		budget = min(budget, wholeMB<<20)
	}
	return budget
}
