package vc

import (
	"context"
	"fmt"

	"github.com/pkg/errors"

	"go.viam.com/rdk/services/navigation"
)

// NMEA 2000 "Route and WP Service" transfer of the active waypoint list.
//
// The nav service doesn't own a CAN interface; it delegates to a configured
// generic sender component (`n2k_sender`, e.g. viam-labs:viamboat:sender)
// whose DoCommand accepts {"pgn", "priority", "dst", "fields"} and marshals
// the fields with canboat field names. A transfer is one PGN 130066
// (Route/WP-List Attributes) announcing the route, followed by PGN 130067
// (Route - WP Name & Position) messages carrying the waypoints, chunked so
// each message fits in a single NMEA 2000 fast-packet.

const (
	pgnRouteWPListAttributes = 130066
	pgnRouteWPNamePosition   = 130067

	// routeWPPriority: route transfer is not time-critical, so use the
	// lowest CAN priority.
	routeWPPriority = 7

	// fastPacketMaxBytes is the max marshaled size of one fast-packet PGN
	// (6 bytes in frame 0 + 7 bytes in each of 31 continuation frames).
	fastPacketMaxBytes = 223
	// wpListHeaderBytes: Start RPS# (1) + nItems (1) + Number of WPs (2) +
	// Database ID (1) + Route ID (1).
	wpListHeaderBytes = 6
	// wpFixedBytes is the per-waypoint wire size excluding the name text:
	// WP ID (1) + latitude (4) + longitude (4) + string-LAU length/control
	// bytes (2).
	wpFixedBytes = 11

	// maxN2KWaypoints: WP IDs in PGN 130067 are single bytes and the top
	// values (253-255) are reserved in NMEA 2000.
	maxN2KWaypoints = 252

	defaultN2KRouteName = "Chartplotter"
	defaultN2KDatabase  = 1
	defaultN2KRoute     = 1
)

type n2kRouteOptions struct {
	RouteName  string
	DatabaseID int
	RouteID    int
	// Dst < 0 means leave it to the sender's default (broadcast).
	Dst int
}

// parseN2KRouteOptions accepts the send_waypoints_n2k payload: either a
// non-object truthy value for all defaults, or an options object. Numbers
// arrive as float64 when the command crosses gRPC.
func parseN2KRouteOptions(raw interface{}) (n2kRouteOptions, error) {
	opts := n2kRouteOptions{
		RouteName:  defaultN2KRouteName,
		DatabaseID: defaultN2KDatabase,
		RouteID:    defaultN2KRoute,
		Dst:        -1,
	}
	m, ok := raw.(map[string]interface{})
	if !ok {
		return opts, nil
	}
	if v, ok := m["route_name"].(string); ok && v != "" {
		opts.RouteName = v
	}
	for key, target := range map[string]*int{
		"database_id": &opts.DatabaseID,
		"route_id":    &opts.RouteID,
		"dst":         &opts.Dst,
	} {
		v, present := m[key]
		if !present {
			continue
		}
		switch n := v.(type) {
		case float64:
			*target = int(n)
		case int:
			*target = n
		default:
			return opts, errors.Errorf("send_waypoints_n2k.%s must be a number, got %T", key, v)
		}
	}
	if opts.DatabaseID < 0 || opts.DatabaseID > 252 {
		return opts, errors.Errorf("send_waypoints_n2k.database_id out of range: %d", opts.DatabaseID)
	}
	if opts.RouteID < 0 || opts.RouteID > 252 {
		return opts, errors.Errorf("send_waypoints_n2k.route_id out of range: %d", opts.RouteID)
	}
	if opts.Dst > 255 {
		return opts, errors.Errorf("send_waypoints_n2k.dst out of range: %d", opts.Dst)
	}
	return opts, nil
}

func n2kWaypointName(i int) string {
	return fmt.Sprintf("WP%03d", i+1)
}

// n2kSenderCmd wraps one PGN's fields into the sender component's DoCommand
// shape.
func n2kSenderCmd(pgn int, opts n2kRouteOptions, fields map[string]interface{}) map[string]interface{} {
	cmd := map[string]interface{}{
		"pgn":      pgn,
		"priority": routeWPPriority,
		"fields":   fields,
	}
	if opts.Dst >= 0 {
		cmd["dst"] = opts.Dst
	}
	return cmd
}

// n2kRouteMessages builds the ordered sender commands that transfer the given
// waypoint list: one 130066 attributes message, then 130067 name & position
// messages chunked to fit a fast-packet each. Pure — no I/O — so the wire
// content is unit-testable.
func n2kRouteMessages(wps []navigation.Waypoint, opts n2kRouteOptions) ([]map[string]interface{}, error) {
	if len(wps) == 0 {
		return nil, errors.New("no waypoints to send")
	}
	if len(wps) > maxN2KWaypoints {
		return nil, errors.Errorf("too many waypoints for one NMEA 2000 route: %d > %d", len(wps), maxN2KWaypoints)
	}

	total := len(wps)
	msgs := []map[string]interface{}{
		n2kSenderCmd(pgnRouteWPListAttributes, opts, map[string]interface{}{
			"Database ID":                        opts.DatabaseID,
			"Route ID":                           opts.RouteID,
			"Route/WP-List Name":                 opts.RouteName,
			"Number of WPs in the Route/WP-List": total,
			// Remaining attribute fields (timestamps, navigation method,
			// route status, XTE limit) are omitted on purpose: the canboat
			// marshaller writes them as "unavailable".
		}),
	}

	budget := fastPacketMaxBytes - wpListHeaderBytes
	start := 0
	for start < total {
		used := 0
		list := []interface{}{}
		for i := start; i < total; i++ {
			name := n2kWaypointName(i)
			need := wpFixedBytes + len(name)
			if used+need > budget {
				break
			}
			used += need
			list = append(list, map[string]interface{}{
				"WP ID":        i,
				"WP Name":      name,
				"WP Latitude":  wps[i].Lat,
				"WP Longitude": wps[i].Long,
			})
		}
		msgs = append(msgs, n2kSenderCmd(pgnRouteWPNamePosition, opts, map[string]interface{}{
			"Start RPS#":                         start,
			"nItems":                             len(list),
			"Number of WPs in the Route/WP-List": total,
			"Database ID":                        opts.DatabaseID,
			"Route ID":                           opts.RouteID,
			"list":                               list,
		}))
		start += len(list)
	}
	return msgs, nil
}

// doSendWaypointsN2K handles {"send_waypoints_n2k": {...}}: it snapshots the
// current (unvisited) waypoint list and pushes it out the configured
// n2k_sender as an NMEA 2000 Route and WP Service transfer.
func (s *navService) doSendWaypointsN2K(ctx context.Context, raw interface{}) (map[string]interface{}, error) {
	if s.n2kSender == nil {
		return nil, errors.New("send_waypoints_n2k: nav service has no n2k_sender configured")
	}
	opts, err := parseN2KRouteOptions(raw)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	wps, err := s.store.Waypoints(ctx)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	msgs, err := n2kRouteMessages(wps, opts)
	if err != nil {
		return nil, err
	}
	for i, msg := range msgs {
		if _, err := s.n2kSender.DoCommand(ctx, msg); err != nil {
			return nil, errors.Wrapf(err, "send_waypoints_n2k: sender failed on message %d of %d (pgn %v)",
				i+1, len(msgs), msg["pgn"])
		}
	}
	s.logger.Infof("sent %d waypoints as n2k route %q (%d messages)", len(wps), opts.RouteName, len(msgs))
	return map[string]interface{}{
		"ok":         true,
		"waypoints":  len(wps),
		"messages":   len(msgs),
		"route_name": opts.RouteName,
	}, nil
}
