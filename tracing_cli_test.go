package seth_test

import (
	"os"
	"testing"

	"github.com/smartcontractkit/seth"
	sethcmd "github.com/smartcontractkit/seth/cmd"
	"github.com/stretchr/testify/require"
)

func TestCLITracing(t *testing.T) {
	c := newClientWithContractMapFromEnv(t)
	SkipAnvil(t, c)

	file, err := os.CreateTemp("", "reverted_transactions.json")
	require.NoError(t, err, "should have created temp file")

	tx, txErr := TestEnv.DebugContract.AlwaysRevertsCustomError(c.NewTXOpts())
	require.NoError(t, txErr, "transaction should have reverted")

	err = seth.CreateOrAppendToJsonArray(file.Name(), tx.Hash().Hex())
	require.NoError(t, err, "should have written to file")

	_ = os.Setenv("SETH_CONFIG_FILE", "seth.toml")
	err = sethcmd.RunCLI([]string{"seth", "-n", os.Getenv("NETWORK"), "trace", "-f", file.Name()})
	require.NoError(t, err, "should have traced transactions")
}
