package app

import (
	"context"
	"testing"
)

// ix.12.13: when Telegram cannot list a forum's topics (a frozen account),
// the topic filter is offered the topics the archive recorded.
func TestIx1213ArchiveTopicsFallback(t *testing.T) {
	ctx := context.Background()
	a, err := newConfiguredTestApp(ctx, Config{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const g = "-1000000000777"
	for _, q := range []string{
		`INSERT INTO ix_topics(group_id,topic_id,title) VALUES('` + g + `',5,'Invented Topic')`,
		`INSERT INTO ix_messages(group_id,message_id,topic_id) VALUES('` + g + `',10,5),('` + g + `',11,9),('` + g + `',12,0),('-1000000000778',13,4)`,
	} {
		if _, err = a.db.Writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got := a.ixArchiveTopics(ctx, g)
	if len(got) != 2 || got[0].ID != 5 || got[0].Name != "Invented Topic" || got[1].ID != 9 || got[1].Name != "Topic 9" {
		t.Fatalf("topics: %+v", got)
	}
	if n := len(a.ixArchiveTopics(ctx, "-1000000000999")); n != 0 {
		t.Fatalf("a group without topics: %d", n)
	}
}
