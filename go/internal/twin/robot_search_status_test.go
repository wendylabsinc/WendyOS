package twin

import (
	"github.com/wendylabsinc/wendy/go/internal/twinrobot"
	"testing"
)

func TestRobotStatusPreservesSearchBetweenViews(t *testing.T) {
	session := &robotSession{searchState: "searching", searchMessage: "Comparing view", searchTurns: 2, searchMoves: 1}
	session.activeObject = &activeObjectJob{result: twinrobot.CommandResult{State: "running", ObjectSearch: &twinrobot.ObjectSearchProgress{Query: "door", Views: 2}}}
	got := robotSearchStatus(session)
	search := got["localization_search"].(map[string]any)
	if search["state"] != "searching" || search["turns"] != 2 || search["moves"] != 1 {
		t.Fatal("lost active desktop search", got)
	}
	object := got["object_search"].(twinrobot.CommandResult)
	if object.State != "running" || object.ObjectSearch.Query != "door" {
		t.Fatal("lost object mission", got)
	}
	object.ObjectSearch.Query = "changed"
	if session.activeObject.result.ObjectSearch.Query != "door" {
		t.Fatal("status aliases mutable progress")
	}
}
