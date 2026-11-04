package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestZZTime(t *testing.T) {
	withLane(t, 2)
	s := testStore(t)
	tx, _ := s.writer.Begin()
	for i := 1; i <= 50000; i++ {
		tx.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,ttl_seconds,created_at,updated_at)
		VALUES('one','-1001','G',?,1,?,'video','f.mp4',?,X'00','pending','history',0,1,1)`, i, fmt.Sprint(i), int64(1<<30)+int64(i))
	}
	tx.Commit()
	s.writer.Exec(`UPDATE tgdl_work SET status='processing' WHERE id<=4`)
	for _, acc := range [][]string{{"one"}, {"one", "two"}} {
		st := time.Now()
		for i := 0; i < 20; i++ {
			w, err := s.Claim(context.Background(), acc, time.Now(), true)
			if err != nil || w != nil {
				t.Fatal(err, w)
			}
		}
		fmt.Println(len(acc), time.Since(st)/20)
	}
}
