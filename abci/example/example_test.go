package example

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/tendermint/tendermint/libs/log"
	tmnet "github.com/tendermint/tendermint/libs/net"

	abciclient "github.com/tendermint/tendermint/abci/client"
	"github.com/tendermint/tendermint/abci/example/code"
	"github.com/tendermint/tendermint/abci/example/kvstore"
	abciserver "github.com/tendermint/tendermint/abci/server"
	"github.com/tendermint/tendermint/abci/types"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

func TestKVStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("### Testing KVStore")
	testStream(ctx, t, kvstore.NewApplication())
}

func TestBaseApp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fmt.Println("### Testing BaseApp")
	testStream(ctx, t, types.NewBaseApplication())
}

func TestGRPC(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("### Testing GRPC")
	testGRPCSync(ctx, t, types.NewGRPCApplication(types.NewBaseApplication()))
}

func testStream(ctx context.Context, t *testing.T, app types.Application) {
	t.Helper()

	const numDeliverTxs = 20000
	socketFile := fmt.Sprintf("test-%08x.sock", rand.Int31n(1<<30))
	defer os.Remove(socketFile)
	socket := fmt.Sprintf("unix://%v", socketFile)
	logger := log.TestingLogger()
	// Start the listener
	server := abciserver.NewSocketServer(logger.With("module", "abci-server"), socket, app)
	t.Cleanup(server.Wait)
	err := server.Start(ctx)
	require.NoError(t, err)

	// Connect to the socket
	client := abciclient.NewSocketClient(log.TestingLogger().With("module", "abci-client"), socket, false)
	t.Cleanup(client.Wait)

	err = client.Start(ctx)
	require.NoError(t, err)

	done := make(chan struct{})
	counter := 0
	client.SetResponseCallback(func(req *types.Request, res *types.Response) {
		// Process response
		switch r := res.Value.(type) {
		case *types.Response_DeliverTx:
			counter++
			if r.DeliverTx.Code != code.CodeTypeOK {
				t.Error("DeliverTx failed with ret_code", r.DeliverTx.Code)
			}
			if counter > numDeliverTxs {
				t.Fatalf("Too many DeliverTx responses. Got %d, expected %d", counter, numDeliverTxs)
			}
			if counter == numDeliverTxs {
				go func() {
					time.Sleep(time.Second * 1) // Wait for a bit to allow counter overflow
					close(done)
				}()
				return
			}
		case *types.Response_Flush:
			// ignore
		default:
			t.Error("Unexpected response type", reflect.TypeOf(res.Value))
		}
	})

	// Write requests
	for counter := 0; counter < numDeliverTxs; counter++ {
		// Send request
		_, err = client.DeliverTxAsync(ctx, types.RequestDeliverTx{Tx: []byte("test")})
		require.NoError(t, err)

		// Sometimes send flush messages
		if counter%128 == 0 {
			err = client.FlushSync(context.Background())
			require.NoError(t, err)
		}
	}

	// Send final flush message
	_, err = client.FlushAsync(ctx)
	require.NoError(t, err)

	<-done
}

//-------------------------
// test grpc

func dialerFunc(ctx context.Context, addr string) (net.Conn, error) {
	return tmnet.Connect(addr)
}

func testGRPCSync(ctx context.Context, t *testing.T, app types.ABCIApplicationServer) {
	numDeliverTxs := 2000
	socketFile := fmt.Sprintf("/tmp/test-%08x.sock", rand.Int31n(1<<30))
	defer os.Remove(socketFile)
	socket := fmt.Sprintf("unix://%v", socketFile)
	logger := log.TestingLogger()
	// Start the listener
	server := abciserver.NewGRPCServer(logger.With("module", "abci-server"), socket, app)

	if err := server.Start(ctx); err != nil {
		t.Fatalf("Error starting GRPC server: %v", err.Error())
	}

	t.Cleanup(func() { server.Wait() })

	// Connect to the socket
	conn, err := grpc.Dial(socket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialerFunc),
	)
	if err != nil {
		t.Fatalf("Error dialing GRPC server: %v", err.Error())
	}

	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})

	client := types.NewABCIApplicationClient(conn)

	// Write requests
	for counter := 0; counter < numDeliverTxs; counter++ {
		// Send request
		response, err := client.DeliverTx(context.Background(), &types.RequestDeliverTx{Tx: []byte("test")})
		if err != nil {
			t.Fatalf("Error in GRPC DeliverTx: %v", err.Error())
		}
		counter++
		if response.Code != code.CodeTypeOK {
			t.Error("DeliverTx failed with ret_code", response.Code)
		}
		if counter > numDeliverTxs {
			t.Fatal("Too many DeliverTx responses")
		}
		t.Log("response", counter)
		if counter == numDeliverTxs {
			go func() {
				time.Sleep(time.Second * 1) // Wait for a bit to allow counter overflow
			}()
		}

	}
}
