package management

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// GetCodexTurnTickets exposes diagnostics without opaque tickets or proxy credentials.
func (h *Handler) GetCodexTurnTickets(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"tickets": helps.DefaultCodexTurnTickets.Snapshot()})
}
