package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/work-frame/goflow/internal/auth"
)

// UserStore is what the API needs from account storage.
// database.UserStore satisfies it in production.
type UserStore interface {
	Create(ctx context.Context, u *auth.User) error
}

type registerRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// userResponse is the public view of a user. It is a separate type so a field
// added to auth.User later can't reach clients by accident.
type userResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeRequestError(w, err)
		return
	}

	// The password lives only in req and in this call. It is never logged,
	// stored, or put into an error message.
	u, err := auth.NewUser(req.Email, req.Password, h.hasher)
	switch {
	case errors.Is(err, auth.ErrInvalidEmail):
		writeError(w, http.StatusBadRequest, "email is invalid")
		return
	case errors.Is(err, auth.ErrPasswordTooShort):
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("password must be at least %d characters", auth.MinPasswordLength))
		return
	case errors.Is(err, auth.ErrPasswordTooLong):
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("password must be at most %d bytes", auth.MaxPasswordBytes))
		return
	case err != nil:
		h.internalError(w, r, err)
		return
	}

	ctx, cancel := dbContext(r)
	defer cancel()

	err = h.users.Create(ctx, u)
	if errors.Is(err, auth.ErrEmailTaken) {
		writeError(w, http.StatusConflict, "email is already registered")
		return
	}
	if err != nil {
		h.storeError(w, r, err)
		return
	}

	writeJSON(w, http.StatusCreated, userResponse{ID: u.ID, Email: u.Email, CreatedAt: u.CreatedAt})
}
