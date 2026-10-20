package upnp

import (
	"context"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kenyonj/airbridge/internal/state"
)

// Player interface for audio playback backends.
type Player interface {
	Play(ctx context.Context, uri string, volume int) error
	Pause(ctx context.Context) error
	Stop(ctx context.Context) error
	SetVolume(ctx context.Context, volume int) error
	// Seek repositions the playhead. Backends that cannot seek in-stream
	// restart their session at the requested offset.
	Seek(ctx context.Context, position time.Duration) error
	// Position reports (elapsed, total); total is 0 when unknown.
	Position() (time.Duration, time.Duration)
}

// AVTransportHandler handles AVTransport SOAP requests.
func AVTransportHandler(st *state.PlayerState, player Player, em *EventManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := st.Context()
		action := ParseSOAPAction(r.Header.Get("SOAPACTION"))
		body, _ := io.ReadAll(r.Body)
		controller := ControllerID(r)

		log.Printf("AVTransport action=%s from=%s", action, controller)

		switch action {
		case "SetAVTransportURI":
			if !st.AcquireOrCheckSession(controller, true) {
				WriteSOAPError(w, 712, "Session in use")
				return
			}
			uri := XMLText(body, "CurrentURI")
			meta := XMLText(body, "CurrentURIMetaData")
			prevURI, _ := st.GetURI()
			st.SetURI(uri, meta)
			log.Printf("SetAVTransportURI: %s", uri)

			// A new URI invalidates a paused stream: without this, the next
			// Play would "resume" the previous track (resume is only valid for
			// the media that is actually loaded).
			if prevURI != "" && uri != prevURI {
				log.Printf("SetAVTransportURI: media changed, dropping previous stream")
				st.SetTransportState(state.StateStopped)
				em.NotifyTransportState(string(state.StateStopped))
				go func(p Player, ctx context.Context) {
					if err := p.Stop(ctx); err != nil {
						log.Printf("Stop previous stream: %v", err)
					}
				}(player, ctx)
			}
			WriteSOAPResponse(w, AVTransportType, "SetAVTransportURIResponse", "")

		case "Play":
			if !st.AcquireOrCheckSession(controller, true) {
				WriteSOAPError(w, 712, "Session in use")
				return
			}
			uri, _ := st.GetURI()
			if uri == "" {
				WriteSOAPError(w, 714, "No content selected")
				return
			}

			// A Play after a pause resumes the existing stream instead of
			// restarting it from the beginning.
			if ip, ok := player.(interface{ IsPaused() bool }); ok && ip.IsPaused() {
				if up, ok2 := player.(interface{ Unpause(context.Context) error }); ok2 {
					st.SetTransportState(state.StatePlaying)
					em.NotifyTransportState(string(state.StatePlaying))
					go func() {
						if err := up.Unpause(ctx); err != nil {
							log.Printf("Unpause error: %v", err)
						}
					}()
					WriteSOAPResponse(w, AVTransportType, "PlayResponse", "")
					return
				}
			}

			// Start playback asynchronously
			go func() {
				st.SetTransportState(state.StateTransition)
				em.NotifyTransportState(string(state.StateTransition))
				if err := player.Play(ctx, uri, st.GetVolume()); err != nil {
					log.Printf("Play error: %v", err)
					st.SetTransportState(state.StateStopped)
					em.NotifyTransportState(string(state.StateStopped))
					return
				}
				st.SetTransportState(state.StatePlaying)
				em.NotifyTransportState(string(state.StatePlaying))
			}()
			WriteSOAPResponse(w, AVTransportType, "PlayResponse", "")

		case "Pause":
			if !st.HasSession(controller) {
				WriteSOAPError(w, 712, "Session in use")
				return
			}
			// Update state and notify immediately (before async player operation)
			st.SetTransportState(state.StatePaused)
			em.NotifyTransportState(string(state.StatePaused))
			go func() {
				if err := player.Pause(ctx); err != nil {
					log.Printf("Pause error: %v", err)
				}
			}()
			WriteSOAPResponse(w, AVTransportType, "PauseResponse", "")

		case "Stop":
			if !st.HasSession(controller) {
				WriteSOAPError(w, 712, "Session in use")
				return
			}
			// Update state and notify immediately (before async player operation)
			st.SetTransportState(state.StateStopped)
			em.NotifyTransportState(string(state.StateStopped))
			go func() {
				if err := player.Stop(ctx); err != nil {
					log.Printf("Stop error: %v", err)
				}
				st.ReleaseSession()
			}()
			WriteSOAPResponse(w, AVTransportType, "StopResponse", "")

		case "Seek":
			if !st.HasSession(controller) {
				WriteSOAPError(w, 712, "Session in use")
				return
			}
			unit := XMLText(body, "Unit")
			if unit == "" {
				unit = "REL_TIME"
			}
			if unit != "REL_TIME" && unit != "ABS_TIME" {
				WriteSOAPError(w, 710, "Seek mode not supported")
				return
			}
			pos, err := parseSeekTarget(XMLText(body, "Target"))
			if err != nil {
				log.Printf("Seek: bad target %q: %v", XMLText(body, "Target"), err)
				WriteSOAPError(w, 711, "Illegal seek target")
				return
			}
			log.Printf("AVTransport Seek to %s", formatUPnPTime(pos))
			st.SetTransportState(state.StateTransition)
			em.NotifyTransportState(string(state.StateTransition))
			go func() {
				if err := player.Seek(ctx, pos); err != nil {
					log.Printf("Seek error: %v", err)
				}
				st.SetTransportState(state.StatePlaying)
				em.NotifyTransportState(string(state.StatePlaying))
			}()
			WriteSOAPResponse(w, AVTransportType, "SeekResponse", "")

		case "GetTransportInfo":
			ts := st.GetTransportState()
			resp := fmt.Sprintf(
				"<CurrentTransportState>%s</CurrentTransportState>"+
					"<CurrentTransportStatus>OK</CurrentTransportStatus>"+
					"<CurrentSpeed>1</CurrentSpeed>",
				ts)
			WriteSOAPResponse(w, AVTransportType, "GetTransportInfoResponse", resp)

		case "GetPositionInfo":
			uri, _ := st.GetURI()
			elapsed, total := player.Position()
			track := 1
			if uri == "" {
				track = 0
			}
			resp := fmt.Sprintf(
				"<Track>%d</Track>"+
					"<TrackDuration>%s</TrackDuration>"+
					"<TrackMetaData></TrackMetaData>"+
					"<TrackURI>%s</TrackURI>"+
					"<RelTime>%s</RelTime>"+
					"<AbsTime>%s</AbsTime>"+
					"<RelCount>0</RelCount>"+
					"<AbsCount>0</AbsCount>",
				track, formatUPnPTime(total), html.EscapeString(uri),
				formatUPnPTime(elapsed), formatUPnPTime(elapsed))
			WriteSOAPResponse(w, AVTransportType, "GetPositionInfoResponse", resp)

		case "GetMediaInfo":
			uri, meta := st.GetURI()
			_, total := player.Position()
			resp := fmt.Sprintf(
				"<NrTracks>1</NrTracks>"+
					"<MediaDuration>%s</MediaDuration>"+
					"<CurrentURI>%s</CurrentURI>"+
					"<CurrentURIMetaData>%s</CurrentURIMetaData>"+
					"<NextURI></NextURI>"+
					"<NextURIMetaData></NextURIMetaData>"+
					"<PlayMedium>NETWORK</PlayMedium>"+
					"<RecordMedium>NOT_IMPLEMENTED</RecordMedium>"+
					"<WriteStatus>NOT_IMPLEMENTED</WriteStatus>",
				formatUPnPTime(total), html.EscapeString(uri), html.EscapeString(meta))
			WriteSOAPResponse(w, AVTransportType, "GetMediaInfoResponse", resp)

		case "GetTransportSettings":
			WriteSOAPResponse(w, AVTransportType, "GetTransportSettingsResponse",
				"<PlayMode>NORMAL</PlayMode><RecQualityMode>NOT_IMPLEMENTED</RecQualityMode>")

		case "GetDeviceCapabilities":
			WriteSOAPResponse(w, AVTransportType, "GetDeviceCapabilitiesResponse",
				"<PlayMedia>NETWORK</PlayMedia><RecMedia>NOT_IMPLEMENTED</RecMedia><RecQualityModes>NOT_IMPLEMENTED</RecQualityModes>")

		default:
			WriteSOAPError(w, 401, "Invalid Action")
		}
	}
}

// RenderingControlHandler handles RenderingControl SOAP requests.
func RenderingControlHandler(st *state.PlayerState, player Player) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := st.Context()
		action := ParseSOAPAction(r.Header.Get("SOAPACTION"))
		body, _ := io.ReadAll(r.Body)
		controller := ControllerID(r)

		log.Printf("RenderingControl action=%s from=%s", action, controller)

		switch action {
		case "SetVolume":
			if !st.HasSession(controller) {
				WriteSOAPError(w, 712, "Session in use")
				return
			}
			vStr := XMLText(body, "DesiredVolume")
			v := 80
			_, _ = fmt.Sscanf(vStr, "%d", &v)
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			st.SetVolume(v)
			if err := player.SetVolume(ctx, v); err != nil {
				log.Printf("SetVolume error: %v", err)
			}
			WriteSOAPResponse(w, RenderingType, "SetVolumeResponse", "")

		case "GetVolume":
			v := st.GetVolume()
			log.Printf("RenderingControl GetVolume: returning %d", v)
			WriteSOAPResponse(w, RenderingType, "GetVolumeResponse",
				fmt.Sprintf("<CurrentVolume>%d</CurrentVolume>", v))

		case "SetMute":
			if !st.HasSession(controller) {
				WriteSOAPError(w, 712, "Session in use")
				return
			}
			mStr := XMLText(body, "DesiredMute")
			m := mStr == "1" || mStr == "true"
			st.SetMute(m)
			WriteSOAPResponse(w, RenderingType, "SetMuteResponse", "")

		case "GetMute":
			m := st.GetMute()
			val := "0"
			if m {
				val = "1"
			}
			log.Printf("RenderingControl GetMute: returning %s", val)
			WriteSOAPResponse(w, RenderingType, "GetMuteResponse",
				fmt.Sprintf("<CurrentMute>%s</CurrentMute>", val))

		default:
			WriteSOAPError(w, 401, "Invalid Action")
		}
	}
}

// ConnectionManagerHandler handles ConnectionManager SOAP requests.
func ConnectionManagerHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		action := ParseSOAPAction(r.Header.Get("SOAPACTION"))

		log.Printf("ConnectionManager action=%s", action)

		switch action {
		case "GetProtocolInfo":
			// Audio formats we can accept
			sink := "http-get:*:audio/mpeg:*," +
				"http-get:*:audio/mp3:*," +
				"http-get:*:audio/wav:*," +
				"http-get:*:audio/x-wav:*," +
				"http-get:*:audio/flac:*," +
				"http-get:*:audio/x-flac:*," +
				"http-get:*:audio/ogg:*," +
				"http-get:*:audio/aac:*," +
				"http-get:*:audio/mp4:*," +
				"http-get:*:audio/L16:*," +
				"http-get:*:audio/*:*"
			WriteSOAPResponse(w, ConnectionType, "GetProtocolInfoResponse",
				fmt.Sprintf("<Source></Source><Sink>%s</Sink>", sink))

		case "GetCurrentConnectionIDs":
			WriteSOAPResponse(w, ConnectionType, "GetCurrentConnectionIDsResponse",
				"<ConnectionIDs>0</ConnectionIDs>")

		case "GetCurrentConnectionInfo":
			resp := "<RcsID>0</RcsID>" +
				"<AVTransportID>0</AVTransportID>" +
				"<ProtocolInfo></ProtocolInfo>" +
				"<PeerConnectionManager></PeerConnectionManager>" +
				"<PeerConnectionID>-1</PeerConnectionID>" +
				"<Direction>Input</Direction>" +
				"<Status>OK</Status>"
			WriteSOAPResponse(w, ConnectionType, "GetCurrentConnectionInfoResponse", resp)

		default:
			WriteSOAPError(w, 401, "Invalid Action")
		}
	}
}

// EventHandlerFor creates an event handler for a specific service.
func EventHandlerFor(em *EventManager, serviceID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "SUBSCRIBE":
			// Check if this is a renewal (has SID header)
			if sid := r.Header.Get("SID"); sid != "" {
				// Renewal - just return OK with same SID
				w.Header().Set("SID", sid)
				w.Header().Set("TIMEOUT", "Second-1800")
				w.WriteHeader(200)
				log.Printf("Event subscription renewed: SID=%s", sid)
				return
			}
			// New subscription
			sid, timeout, err := em.Subscribe(r, serviceID)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			w.Header().Set("SID", sid)
			w.Header().Set("TIMEOUT", fmt.Sprintf("Second-%d", timeout))
			w.WriteHeader(200)

		case "UNSUBSCRIBE":
			sid := r.Header.Get("SID")
			if sid != "" {
				em.Unsubscribe(sid)
			}
			w.WriteHeader(200)

		default:
			w.WriteHeader(405)
		}
	}
}

// EventHandlerWithState creates an event handler that sends initial state on subscribe.
func EventHandlerWithState(em *EventManager, serviceID string, st *state.PlayerState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "SUBSCRIBE":
			// Check if this is a renewal (has SID header)
			if sid := r.Header.Get("SID"); sid != "" {
				// Renewal - just return OK with same SID
				w.Header().Set("SID", sid)
				w.Header().Set("TIMEOUT", "Second-1800")
				w.WriteHeader(200)
				log.Printf("Event subscription renewed: SID=%s", sid)
				return
			}
			// New subscription
			sid, timeout, err := em.Subscribe(r, serviceID)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			w.Header().Set("SID", sid)
			w.Header().Set("TIMEOUT", fmt.Sprintf("Second-%d", timeout))
			w.WriteHeader(200)

			// Send initial state event asynchronously
			go func() {
				if serviceID == "avtransport" {
					em.NotifyTransportState(string(st.GetTransportState()))
				} else if serviceID == "renderingcontrol" {
					em.NotifyVolume(st.GetVolume(), st.GetMute())
				}
			}()

		case "UNSUBSCRIBE":
			sid := r.Header.Get("SID")
			if sid != "" {
				em.Unsubscribe(sid)
			}
			w.WriteHeader(200)

		default:
			w.WriteHeader(405)
		}
	}
}

// formatUPnPTime renders a duration as HH:MM:SS, the format UPnP expects for
// TrackDuration / RelTime / MediaDuration.
func formatUPnPTime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d / time.Second)
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

// parseSeekTarget accepts HH:MM:SS, MM:SS or a bare second count, with an
// optional fractional last component (some control points send 12.5).
func parseSeekTarget(v string) (time.Duration, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, fmt.Errorf("empty target")
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, fmt.Errorf("invalid target %q", v)
	}
	nums := make([]float64, len(parts))
	for i, p := range parts {
		f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("invalid target %q", v)
		}
		nums[i] = f
	}
	switch len(nums) {
	case 1:
		return time.Duration(nums[0] * float64(time.Second)), nil
	case 2:
		return time.Duration(nums[0])*time.Minute +
			time.Duration(nums[1]*float64(time.Second)), nil
	default:
		return time.Duration(nums[0])*time.Hour +
			time.Duration(nums[1])*time.Minute +
			time.Duration(nums[2]*float64(time.Second)), nil
	}
}
