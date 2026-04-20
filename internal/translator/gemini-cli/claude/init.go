package claude

import (
	. "github.com/assast/CLIProxyAPI/v6/internal/constant"
	"github.com/assast/CLIProxyAPI/v6/internal/interfaces"
	"github.com/assast/CLIProxyAPI/v6/internal/translator/translator"
)

func init() {
	translator.Register(
		Claude,
		GeminiCLI,
		ConvertClaudeRequestToCLI,
		interfaces.TranslateResponse{
			Stream:     ConvertGeminiCLIResponseToClaude,
			NonStream:  ConvertGeminiCLIResponseToClaudeNonStream,
			TokenCount: ClaudeTokenCount,
		},
	)
}
