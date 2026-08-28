package account

import "errors"

var (
	ErrNotFound            = errors.New("account not found")
	ErrDuplicateRemoteUser = errors.New("remote user ID is already bound to another local account")
)

// Repository is the account registry used by the services, the scheduler and
// the web handlers. Implementations persist only sanitized metadata.
type Repository interface {
	List() ([]Record, error)
	ListEnabled() ([]Record, error)
	Get(id string) (Record, error)
	Create(record Record) (Record, error)
	Update(record Record) (Record, error)
	SetRemoteUserID(id string, userID int64) error
	Delete(id string) error
}
