package kindtest

import (
	"github.com/tycheg/reasonr-engine/internal/hub"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// LinkHub is the engine's notification hub over a link between instances
// (port.HubLink), as the server builds it: the tests of a link adapter run
// the hub conformance suite on it.
func LinkHub(link port.HubLink) kind.NotificationHub { return hub.New(link, nil) }
