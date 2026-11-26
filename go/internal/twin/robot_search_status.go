package twin

// Controller idleness between views does not mean its search has ended. Report
// the mission state separately, including desktop-owned localization searches.
func robotSearchStatus(session *robotSession) map[string]any {
	out := make(map[string]any)
	session.searchMu.Lock()
	out["localization_search"] = map[string]any{"state": session.searchState, "message": session.searchMessage, "turns": session.searchTurns, "moves": session.searchMoves}
	session.searchMu.Unlock()
	session.activeObjectMu.Lock()
	job := session.activeObject
	session.activeObjectMu.Unlock()
	if job != nil {
		job.mu.Lock()
		result := job.result
		if result.ObjectSearch != nil {
			progress := *result.ObjectSearch
			result.ObjectSearch = &progress
		}
		job.mu.Unlock()
		out["object_search"] = result
	}
	return out
}
