package engine

import (
	"fmt"
	"strconv"
	"testing"
)

func TestZZ(t *testing.T) {
	withLane(t, 2)
	s := testStore(t)
	tx, _ := s.writer.Begin()
	for i := 1; i <= 50000; i++ {
		tx.Exec(`INSERT INTO tgdl_work(account_id,group_id,group_name,message_id,version,identity,media_type,file_name,file_size,body,status,origin,created_at,updated_at)
			VALUES('one','-1001','G',?,1,?,'video','f.mp4',?,X'00',?,'history',1,1)`, i, "id"+strconv.Itoa(i), BigFileSize+int64(i), map[bool]string{true: "processing", false: "pending"}[i <= 4])
	}
	tx.Commit()
	fmt.Println(claimPlan(t, s, []string{"one"}))
}
