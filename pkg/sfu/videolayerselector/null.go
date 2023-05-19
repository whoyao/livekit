package videolayerselector

import (
	"github.com/whoyao/protocol/logger"
)

type Null struct {
	*Base
}

func NewNull(logger logger.Logger) *Null {
	return &Null{
		Base: NewBase(logger),
	}
}
