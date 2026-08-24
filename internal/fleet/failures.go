package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// This file holds the one translation between a failure on another Spark and
// the sentence the console shows for it. A row in the fleet dashboard is read
// by the person who owns the machines, not by the person who wrote the fleet
// protocol, so it names the Spark by the name its owner gave it and says what
// is wrong in plain words. Node ids, addresses and HTTP status numbers stay
// where they belong: in the logs and in the detail of a failure nobody has a
// plainer word for.

// The words a release gap is reported with, wherever it is found. The planner
// writes one of the first three into a candidate reason; the target node
// writes the fourth into the answer it refuses a placement with. They are
// constants so that the console sentence can recognise a release gap without
// guessing at wording that could drift away from it.
const (
	nodeVersionSkew   = "the node manager version does not exactly match the controller"
	nodeBuildSkew     = "the node build identity does not exactly match the controller"
	nodeCatalogueSkew = "the node recipe catalogue does not exactly match the controller"
	nodeReleaseSkew   = "the target node does not exactly match the controller release and catalogue"
)

// nodeUnreachable marks a call that got no answer at all: the address did not
// answer, the connection stopped, or the deadline passed. The text stays the
// client's own, so every log line reads exactly as it did before; only the
// type is new, and only so that silence can be told apart from a refusal.
type nodeUnreachable struct{ err error }

func (failure nodeUnreachable) Error() string { return failure.err.Error() }
func (failure nodeUnreachable) Unwrap() error { return failure.err }

// nodeStatus marks an answer that carried a status and no reason of its own.
// 404, 405 and 501 mean the other manager has no such endpoint, which inside
// one fleet only happens while the two managers run different releases.
type nodeStatus struct{ status int }

func (failure nodeStatus) Error() string {
	return fmt.Sprintf("fleet manager returned status %d", failure.status)
}

// nodeFailure turns one failure on one Spark into the sentence the console
// shows for it. There are three cases and no more: the Spark said nothing,
// the Spark runs a different release of this manager, or the Spark gave a
// reason of its own. Only the third keeps a detail, and it keeps all of it,
// because a reason this code has no plainer word for is still the truth.
func nodeFailure(name string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case nodeIsSilent(err):
		return fmt.Errorf("%s is not answering.", name)
	case nodeIsBehind(err):
		return fmt.Errorf("%s runs another manager version, so update the fleet first.", name)
	}
	return fmt.Errorf("%s could not do this: %s", name, failureDetail(err))
}

// nodeIsSilent reports the case where nothing came back. A Spark that
// answered with a certificate this controller does not trust did answer, and
// saying it is silent would send its owner to look at a cable instead of at
// the identity that changed.
func nodeIsSilent(err error) bool {
	var unreachable nodeUnreachable
	if !errors.As(err, &unreachable) {
		return false
	}
	for _, refusal := range []error{errServerCertificateCount, errServerCertificatePin, errServerCertificateValidity} {
		if errors.Is(err, refusal) {
			return false
		}
	}
	return true
}

// nodeIsBehind reports the case where the two managers do not run the same
// release. A missing endpoint is the clearest evidence of it, because every
// node in one fleet is meant to serve the same protocol.
func nodeIsBehind(err error) bool {
	var status nodeStatus
	if errors.As(err, &status) {
		switch status.status {
		case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
			return true
		}
	}
	text := err.Error()
	for _, skew := range []string{nodeVersionSkew, nodeBuildSkew, nodeCatalogueSkew, nodeReleaseSkew} {
		if strings.Contains(text, skew) {
			return true
		}
	}
	return false
}

// failureDetail is what is left of a failure once the address comes out of
// it. The HTTP client names the URL it called in every transport error, and
// an address is not what the owner asked about.
func failureDetail(err error) string {
	var address *url.Error
	if errors.As(err, &address) && address.Err != nil {
		return address.Err.Error()
	}
	return err.Error()
}

// nodeName is what the console calls a Spark: the name its owner gave it,
// which the fleet table holds for every member and this manager holds for
// itself. A node id is the last resort, because a sentence with no name at
// all is worse than a sentence with an id in it.
func (m *Manager) nodeName(ctx context.Context, nodeID string) string {
	if nodeID == m.identity.NodeID {
		return m.displayName
	}
	nodes, err := m.database.FleetNodes(ctx)
	if err != nil {
		return nodeID
	}
	for _, node := range nodes {
		if node.NodeID == nodeID && strings.TrimSpace(node.DisplayName) != "" {
			return node.DisplayName
		}
	}
	return nodeID
}

// nodeFailure names the Spark before it maps the cause.
func (m *Manager) nodeFailure(ctx context.Context, nodeID string, err error) error {
	return nodeFailure(m.nodeName(ctx, nodeID), err)
}
