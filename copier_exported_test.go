package copier

import (
	"fmt"
	"testing"
)

type User struct {
	State int32
}
type UserPb struct {
	state int32
	State int32
}

func TestCopyExported(t *testing.T) {
	user := User{
		State: 10,
	}
	userPb := UserPb{}
	err := Copy(&userPb, user)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("user pb [%+v]\n", userPb)
}
