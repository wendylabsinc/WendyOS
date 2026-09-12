package data

import (
	"fmt"
	"strconv"
	"strings"
)

// ROS 2 source identifiers.
//
// A ROS 2 graph is addressable at two granularities, and both are real:
//
//	ros2:<rmw>:domain-<n>            the whole DDS domain, recorded with -a
//	ros2:<rmw>:domain-<n>:<topic>    one topic on that domain
//
// The per-topic form is deliberately the domain form with ":" and the topic
// name appended, rather than a new shape such as "ros2:<domain>:<topic>". Two
// reasons. First, one parser reads both, and the domain-level identifier that
// deployed campaigns already name is literally the prefix of every topic
// identifier on that domain, so backwards compatibility is a property of the
// scheme instead of a special case in the resolver. Second, the RMW name has
// to stay in the identifier: two RMW implementations can be live on the same
// DDS domain number and they do not interoperate, so they are different
// graphs, and dropping the RMW would let "/chatter" on one collide with
// "/chatter" on the other. The domain-level identifier has always carried the
// RMW; a per-topic identifier that dropped it would be a regression.
//
// <topic> is the ROS 2 topic name verbatim, leading slash included, and it is
// always the last field. ROS 2 graph names admit only letters, digits,
// underscores and slashes, so a ":" can never occur inside one: everything
// after the third ":" is the topic, however many slashes it contains.
// "ros2:rmw_cyclonedds_cpp:domain-42:/camera/left/image_raw" round-trips to
// the domain identifier "ros2:rmw_cyclonedds_cpp:domain-42" and the topic
// "/camera/left/image_raw".
//
// The grammar lives here, in the package that owns episode sources, because
// two callers depend on it: the agent adapter that mints these identifiers and
// the campaign resolver that matches campaign selectors against them. One
// scheme, one parser.
const ROS2SourcePrefix = "ros2:"

// ROS2DomainSourceID builds the identifier for a whole DDS domain. An empty
// RMW name is normalized to "default" so the identifier always has its three
// fields.
func ROS2DomainSourceID(rmw string, domainID int) string {
	if rmw == "" {
		rmw = "default"
	}
	return fmt.Sprintf("%s%s:domain-%d", ROS2SourcePrefix, safeName(rmw), domainID)
}

// ROS2TopicSourceID builds the identifier for one topic on a DDS domain.
func ROS2TopicSourceID(rmw string, domainID int, topic string) string {
	return ROS2DomainSourceID(rmw, domainID) + ":" + topic
}

// ParseROS2SourceID splits a ROS 2 source identifier into the domain-level
// identifier it belongs to and the topic it names. topic is empty for a
// domain-level identifier, which is what makes the pre-per-topic spelling
// parse rather than needing a compatibility branch.
func ParseROS2SourceID(id string) (domainID, topic string, ok bool) {
	rest, found := strings.CutPrefix(id, ROS2SourcePrefix)
	if !found {
		return "", "", false
	}
	rmw, rest, found := strings.Cut(rest, ":")
	if !found || rmw == "" {
		return "", "", false
	}
	domainField, topic, hasTopic := strings.Cut(rest, ":")
	digits, isDomain := strings.CutPrefix(domainField, "domain-")
	if !isDomain {
		return "", "", false
	}
	if _, err := strconv.Atoi(digits); err != nil {
		return "", "", false
	}
	// A ROS 2 topic name is absolute, so the remainder must start with a
	// slash. This rejects a truncated or hand-typed identifier instead of
	// quietly recording a bag for a topic nobody named.
	if hasTopic && !strings.HasPrefix(topic, "/") {
		return "", "", false
	}
	if !hasTopic {
		topic = ""
	}
	return ROS2SourcePrefix + rmw + ":" + domainField, topic, true
}
