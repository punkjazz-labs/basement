package fleet

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/punkjazz-labs/basement/internal/operations"
	"github.com/punkjazz-labs/basement/internal/recipe"
)

const ClaimsVersion = 1

const (
	ClaimKindLocalJob    = "local-job"
	ClaimKindIndependent = "independent"
	ClaimKindLegacyRank  = "legacy-rank"
	ClaimKindRecovered   = "recovered-model"
)

// Claims is the versioned resource document one local allocator persists.
// Disk is a preparation claim and is counted only before work becomes active.
// Runtime and ports become exclusive when Activate transfers this node's one
// serving slot. Node identity is deliberately absent because every allocator
// governs exactly the database and host on which it runs.
type Claims struct {
	Version          int      `json:"version"`
	Kind             string   `json:"kind"`
	JobID            string   `json:"job_id,omitempty"`
	DiskBytes        int64    `json:"disk_bytes"`
	MemoryBytes      int64    `json:"memory_bytes"`
	Runtime          bool     `json:"runtime"`
	Ports            []int    `json:"ports"`
	FabricInterfaces []string `json:"fabric_interfaces"`
}

type RecipeClaimOptions struct {
	Kind        string
	JobID       string
	ReserveDisk bool
	Runtime     bool
	Placement   operations.Placement
}

// ClaimsForRecipe builds the persistent resource meaning of one exact recipe
// without consulting inventory, time, addresses, or other process state. All
// reservation entry points use this constructor so planning and retrying the
// same work cannot disagree about a port or a per-node memory figure.
func ClaimsForRecipe(selected recipe.Recipe, options RecipeClaimOptions) Claims {
	claims := Claims{
		Version: ClaimsVersion, Kind: options.Kind, JobID: options.JobID,
		Runtime: options.Runtime, Ports: []int{}, FabricInterfaces: []string{},
	}
	if options.ReserveDisk {
		claims.DiskBytes = selected.RequiredBytes()
	}
	if !options.Runtime {
		return claims
	}
	claims.MemoryBytes = RecipeMemoryClaim(selected)
	if port, binds := operations.RankBindsHostPort(selected, options.Placement); binds {
		claims.Ports = []int{port}
	}
	if selected.Distributed() && selected.Topology.SocketInterface() != "" {
		claims.FabricInterfaces = []string{selected.Topology.SocketInterface()}
	}
	return claims
}

func (claims Claims) validate() error {
	if claims.Version != ClaimsVersion {
		return errors.New("reservation claims version is not supported")
	}
	switch claims.Kind {
	case ClaimKindLocalJob, ClaimKindIndependent, ClaimKindLegacyRank, ClaimKindRecovered:
	default:
		return errors.New("reservation claim kind is not supported")
	}
	if claims.DiskBytes < 0 {
		return errors.New("reservation disk bytes cannot be negative")
	}
	if claims.MemoryBytes < 0 {
		return errors.New("reservation memory bytes cannot be negative")
	}
	if !claims.Runtime && len(claims.Ports) > 0 {
		return errors.New("a disk-only reservation cannot claim a serving port")
	}
	if !claims.Runtime && (claims.MemoryBytes > 0 || len(claims.FabricInterfaces) > 0) {
		return errors.New("a disk-only reservation cannot claim serving memory or a fabric interface")
	}
	previous := 0
	for index, port := range claims.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("reservation port %d is invalid", port)
		}
		if index > 0 && port <= previous {
			return errors.New("reservation ports must be unique and sorted")
		}
		previous = port
	}
	previousInterface := ""
	for index, name := range claims.FabricInterfaces {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return errors.New("reservation fabric interfaces cannot be empty or padded")
		}
		if index > 0 && name <= previousInterface {
			return errors.New("reservation fabric interfaces must be unique and sorted")
		}
		previousInterface = name
	}
	return nil
}

func canonicalClaims(claims Claims) Claims {
	claims.Ports = append([]int(nil), claims.Ports...)
	sort.Ints(claims.Ports)
	claims.FabricInterfaces = append([]string(nil), claims.FabricInterfaces...)
	sort.Strings(claims.FabricInterfaces)
	return claims
}

// RecipeMemoryClaim records the recipe's own per-node serving budget. The
// runtime slot remains exclusive, but keeping the concrete budget in the
// signed claim makes admission receipts explain which local memory policy was
// accepted instead of relying on a later catalogue lookup.
func RecipeMemoryClaim(selected recipe.Recipe) int64 {
	if planned, ok := selected.PlannedMemoryBytes(); ok {
		return planned
	}
	if selected.Requirements.MinimumMemoryBytes > selected.Requirements.MemoryReserveBytes {
		return selected.Requirements.MinimumMemoryBytes - selected.Requirements.MemoryReserveBytes
	}
	return selected.Requirements.MinimumMemoryBytes
}
