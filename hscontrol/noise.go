package hscontrol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/juanfont/headscale/hscontrol/capver"
	"github.com/juanfont/headscale/hscontrol/types"
	"github.com/rs/zerolog/log"
	"golang.org/x/net/http2"
	"gorm.io/gorm"
	"tailscale.com/control/controlbase"
	"tailscale.com/control/controlhttp/controlhttpserver"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

const (
	// ts2021UpgradePath is the path that the server listens on for the WebSockets upgrade.
	ts2021UpgradePath = "/ts2021"

	// The first 9 bytes from the server to client over Noise are either an HTTP/2
	// settings frame (a normal HTTP/2 setup) or, as Tailscale added later, an "early payload"
	// header that's also 9 bytes long: 5 bytes (earlyPayloadMagic) followed by 4 bytes
	// of length. Then that many bytes of JSON-encoded tailcfg.EarlyNoise.
	// The early payload is optional. Some servers may not send it... But we do!
	earlyPayloadMagic = "\xff\xff\xffTS"

	// EarlyNoise was added in protocol version 49.
	earlyNoiseCapabilityVersion = 49
)

type noiseServer struct {
	headscale *Headscale

	httpBaseConfig *http.Server
	http2Server    *http2.Server
	conn           *controlbase.Conn
	machineKey     key.MachinePublic
	nodeKey        key.NodePublic

	// EarlyNoise-related stuff
	challenge       key.ChallengePrivate
	protocolVersion int
}

// NoiseUpgradeHandler is to upgrade the connection and hijack the net.Conn
// in order to use the Noise-based TS2021 protocol. Listens in /ts2021.
func (h *Headscale) NoiseUpgradeHandler(
	writer http.ResponseWriter,
	req *http.Request,
) {
	log.Trace().Caller().Msgf("Noise upgrade handler for client %s", req.RemoteAddr)

	upgrade := req.Header.Get("Upgrade")
	if upgrade == "" {
		// This probably means that the user is running Headscale behind an
		// improperly configured reverse proxy. TS2021 requires WebSockets to
		// be passed to Headscale. Let's give them a hint.
		log.Warn().
			Caller().
			Msg("No Upgrade header in TS2021 request. If headscale is behind a reverse proxy, make sure it is configured to pass WebSockets through.")
		http.Error(writer, "Internal error", http.StatusInternalServerError)

		return
	}

	noiseServer := noiseServer{
		headscale: h,
		challenge: key.NewChallenge(),
	}

	noiseConn, err := controlhttpserver.AcceptHTTP(
		req.Context(),
		writer,
		req,
		*h.noisePrivateKey,
		noiseServer.earlyNoise,
	)
	if err != nil {
		httpError(writer, fmt.Errorf("noise upgrade failed: %w", err))
		return
	}

	noiseServer.conn = noiseConn
	noiseServer.machineKey = noiseServer.conn.Peer()
	noiseServer.protocolVersion = noiseServer.conn.ProtocolVersion()

	// This router is served only over the Noise connection, and exposes only the new API.
	//
	// The HTTP2 server that exposes this router is created for
	// a single hijacked connection from /ts2021, using netutil.NewOneConnListener
	router := mux.NewRouter()
	router.Use(prometheusMiddleware)

	router.HandleFunc("/machine/register", noiseServer.NoiseRegistrationHandler).
		Methods(http.MethodPost)
	router.HandleFunc("/machine/map", noiseServer.NoisePollNetMapHandler)

	noiseServer.httpBaseConfig = &http.Server{
		Handler:           router,
		ReadHeaderTimeout: types.HTTPTimeout,
	}
	noiseServer.http2Server = &http2.Server{}

	noiseServer.http2Server.ServeConn(
		noiseConn,
		&http2.ServeConnOpts{
			BaseConfig: noiseServer.httpBaseConfig,
		},
	)
}

func unsupportedClientError(version tailcfg.CapabilityVersion) error {
	return fmt.Errorf("unsupported client version: %s (%d)", capver.TailscaleVersion(version), version)
}

func (ns *noiseServer) earlyNoise(protocolVersion int, writer io.Writer) error {
	if !isSupportedVersion(tailcfg.CapabilityVersion(protocolVersion)) {
		return unsupportedClientError(tailcfg.CapabilityVersion(protocolVersion))
	}

	earlyJSON, err := json.Marshal(&tailcfg.EarlyNoise{
		NodeKeyChallenge: ns.challenge.Public(),
	})
	if err != nil {
		return err
	}

	// 5 bytes that won't be mistaken for an HTTP/2 frame:
	// https://httpwg.org/specs/rfc7540.html#rfc.section.4.1 (Especially not
	// an HTTP/2 settings frame, which isn't of type 'T')
	var notH2Frame [5]byte
	copy(notH2Frame[:], earlyPayloadMagic)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(earlyJSON)))
	// These writes are all buffered by caller, so fine to do them
	// separately:
	if _, err := writer.Write(notH2Frame[:]); err != nil {
		return err
	}
	if _, err := writer.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err := writer.Write(earlyJSON); err != nil {
		return err
	}

	return nil
}

func isSupportedVersion(version tailcfg.CapabilityVersion) bool {
	return version >= capver.MinSupportedCapabilityVersion
}

func rejectUnsupported(
	writer http.ResponseWriter,
	version tailcfg.CapabilityVersion,
	mkey key.MachinePublic,
	nkey key.NodePublic,
) bool {
	// Reject unsupported versions
	if !isSupportedVersion(version) {
		log.Error().
			Caller().
			Int("minimum_cap_ver", int(capver.MinSupportedCapabilityVersion)).
			Int("client_cap_ver", int(version)).
			Str("minimum_version", capver.TailscaleVersion(capver.MinSupportedCapabilityVersion)).
			Str("client_version", capver.TailscaleVersion(version)).
			Str("node_key", nkey.ShortString()).
			Str("machine_key", mkey.ShortString()).
			Msg("unsupported client connected")
		http.Error(writer, unsupportedClientError(version).Error(), http.StatusBadRequest)

		return true
	}

	return false
}

// NoisePollNetMapHandler takes care of /machine/:id/map using the Noise protocol
//
// This is the busiest endpoint, as it keeps the HTTP long poll that updates
// the clients when something in the network changes.
//
// The clients POST stuff like HostInfo and their Endpoints here, but
// only after their first request (marked with the ReadOnly field).
//
// At this moment the updates are sent in a quite horrendous way, but they kinda work.
func (ns *noiseServer) NoisePollNetMapHandler(
	writer http.ResponseWriter,
	req *http.Request,
) {
	body, _ := io.ReadAll(req.Body)

	var mapRequest tailcfg.MapRequest
	if err := json.Unmarshal(body, &mapRequest); err != nil {
		httpError(writer, err)
		return
	}

	// Reject unsupported versions
	if rejectUnsupported(writer, mapRequest.Version, ns.machineKey, mapRequest.NodeKey) {
		return
	}

	ns.nodeKey = mapRequest.NodeKey

	node, err := ns.headscale.db.GetNodeByNodeKey(mapRequest.NodeKey)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			httpError(writer, NewHTTPError(http.StatusNotFound, "node not found", nil))
			return
		}
		httpError(writer, err)
		return
	}

	sess := ns.headscale.newMapSession(req.Context(), mapRequest, writer, node)
	sess.tracef("a node sending a MapRequest with Noise protocol")
	if !sess.isStreaming() {
		sess.serve()
	} else {
		sess.serveLongPoll()
	}
}

// NoiseRegistrationHandler handles the actual registration process of a node.
func (ns *noiseServer) NoiseRegistrationHandler(
	writer http.ResponseWriter,
	req *http.Request,
) {
	if req.Method != http.MethodPost {
		httpError(writer, errMethodNotAllowed)

		return
	}

	registerRequest, registerResponse, err := func() (*tailcfg.RegisterRequest, []byte, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, nil, err
		}
		var registerRequest tailcfg.RegisterRequest
		if err := json.Unmarshal(body, &registerRequest); err != nil {
			return nil, nil, err
		}

		ns.nodeKey = registerRequest.NodeKey

		resp, err := ns.headscale.handleRegister(req.Context(), registerRequest, ns.conn.Peer())
		// TODO(kradalby): Here we could have two error types, one that is surfaced to the client
		// and one that returns 500.
		if err != nil {
			return nil, nil, err
		}

		respBody, err := json.Marshal(resp)
		if err != nil {
			return nil, nil, err
		}

		return &registerRequest, respBody, nil
	}()
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Error handling registration")
		http.Error(writer, "Internal server error", http.StatusInternalServerError)
	}

	// Reject unsupported versions
	if rejectUnsupported(writer, registerRequest.Version, ns.machineKey, registerRequest.NodeKey) {
		return
	}

	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, err = writer.Write(registerResponse)
	if err != nil {
		log.Error().
			Caller().
			Err(err).
			Msg("Failed to write response")
	}
}
