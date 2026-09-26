package client

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/coder/websocket"

	"github.com/wedevwork/callsheet/internal/contract"
)

// maxNodeBody bounds a single-node response.
const maxNodeBody = 64 << 10

// EnrollNode registers id with the plane (idempotent by ID) and returns the
// plane's record, which must name id.
func (c *Client) EnrollNode(ctx context.Context, id, softwareVersion string) (contract.Node, error) {
	body, err := contract.Encode(contract.EnrollRequest{NodeID: id, SoftwareVersion: softwareVersion})
	if err != nil {
		return contract.Node{}, contract.Wrap(contract.CodeInternal, "cannot encode the enrollment request", err)
	}
	status, b, err := c.request(ctx, http.MethodPost, contract.PathEnroll, body, maxNodeBody)
	if err != nil {
		return contract.Node{}, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return contract.Node{}, contract.New(contract.CodeInvalidArgument, "unexpected enrollment response status")
	}
	n, err := contract.ParseNodeResponse(b)
	if err != nil {
		return contract.Node{}, err
	}
	if n.ID != id {
		return contract.Node{}, contract.New(contract.CodeInvalidArgument, "the plane's enrollment response names another node")
	}
	return n, nil
}

// ListNodes returns the complete roster, sorted by ID. The response is
// capped at 8 MiB and rejected whole when oversized or malformed.
func (c *Client) ListNodes(ctx context.Context) ([]contract.Node, error) {
	_, b, err := c.request(ctx, http.MethodGet, contract.PathNodes, nil, maxRosterBody)
	if err != nil {
		return nil, err
	}
	return contract.ParseNodeListResponse(b)
}

// ShowNode returns one node; the response must name id.
func (c *Client) ShowNode(ctx context.Context, id string) (contract.Node, error) {
	if !contract.ValidNodeID(id) {
		return contract.Node{}, contract.New(contract.CodeInvalidArgument, "invalid node ID; want n_ followed by 32 lowercase hex digits")
	}
	_, b, err := c.request(ctx, http.MethodGet, contract.PathNodes+"/"+id, nil, maxNodeBody)
	if err != nil {
		return contract.Node{}, err
	}
	n, err := contract.ParseNodeResponse(b)
	if err != nil {
		return contract.Node{}, err
	}
	if n.ID != id {
		return contract.Node{}, contract.New(contract.CodeInvalidArgument, "the plane's response names another node")
	}
	return n, nil
}

// DialNodeStream opens the verified node stream (no compression, the
// protocol read limit set). The handshake is bounded by OperationTimeout;
// the caller owns the returned connection, its hello and its close. An
// HTTP 503 or any transport loss is unavailable, verification failures
// are trust_failed, and any other refused upgrade is invalid_argument.
func (c *Client) DialNodeStream(ctx context.Context) (*websocket.Conn, error) {
	octx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	conn, resp, err := websocket.Dial(octx, "wss"+c.ep.url[len("https"):]+contract.PathNodeStream, &websocket.DialOptions{
		HTTPClient:      c.http,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if perr := ctx.Err(); perr != nil {
			return nil, perr
		}
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			if resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode >= 500 {
				return nil, contract.Wrap(contract.CodeUnavailable, "the plane refused the node stream with HTTP "+strconv.Itoa(resp.StatusCode), err)
			}
			return nil, contract.Wrap(contract.CodeInvalidArgument, "the plane refused the node stream upgrade with HTTP "+strconv.Itoa(resp.StatusCode)+"; check that the plane and sidecar are the same callsheet version", err)
		}
		var ce *contract.Error
		if errors.As(err, &ce) {
			return nil, ce
		}
		return nil, classify(c.ep, err)
	}
	conn.SetReadLimit(contract.MaxFrameBytes)
	return conn, nil
}
