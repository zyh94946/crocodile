package define

import "fmt"

// ErrUserPass user pass err
type ErrUserPass struct {
	Err error
}

func (u ErrUserPass) Error() string {
	return "username or password error: " + u.Err.Error()
}

// ErrForbid user forbid login err
type ErrForbid struct {
	Name string
}

func (u ErrForbid) Error() string {
	return fmt.Sprintf("user %s forbid login", u.Name)
}