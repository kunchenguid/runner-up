package coordinator

import (
	"os"

	"github.com/kunchenguid/runner-up/internal/config"
	"github.com/kunchenguid/runner-up/internal/plan"
)

func BorrowMain([]string) int                                               { return 2 }
func RunBorrow(*config.Config, string, string) int                          { return 2 }
func (s *Service) refreshBorrowsLocked()                                    {}
func (s *Service) assignBorrowLocked(config.Place, plan.Job, *os.File) bool { return false }
