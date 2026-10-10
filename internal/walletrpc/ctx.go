package walletrpc

import (
	"context"
	"time"
)

func contextTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}
