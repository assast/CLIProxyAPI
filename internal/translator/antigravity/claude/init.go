package claude

import (
	. "github.com/assast/CLIProxyAPI/v6/internal/constant"
	"github.com/assast/CLIProxyAPI/v6/internal/interfaces"
	"github.com/assast/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		Claude,
		Antigravity,
		ConvertClaudeRequestToAntigravity,
		interfaces.TranslateResponse{
			Stream:     ConvertAntigravityResponseToClaude,
			NonStream:  ConvertAntigravityResponseToClaudeNonStream,
			TokenCount: ClaudeTokenCount,
		},
	)
}
