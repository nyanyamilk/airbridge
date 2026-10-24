// Package bridge provides a unified DLNA bridge with embedded renderer devices.
package bridge

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kenyonj/airbridge/internal/cast"
	"github.com/kenyonj/airbridge/internal/database"
	"github.com/kenyonj/airbridge/internal/discovery"
	"github.com/kenyonj/airbridge/internal/httpserver"
	"github.com/kenyonj/airbridge/internal/player"
	"github.com/kenyonj/airbridge/internal/plugins"
	"github.com/kenyonj/airbridge/internal/ssdp"
	"github.com/kenyonj/airbridge/internal/state"
	"github.com/kenyonj/airbridge/internal/upnp"
)

const (
	rootUUID = "airbridge-root-device"
)

// RendererInstance represents a single embedded renderer.
type RendererInstance struct {
	ID           string
	Name         string
	DeviceType   string            // "airplay" or "chromecast"
	AirPlayName  string            // Legacy: for AirPlay devices
	DeviceName   string            // Generic device name
	Device       *discovery.Device // Target device
	Port         int               // HTTP port for this renderer
	State        *state.PlayerState
	Player       upnp.Player // Generic player interface
	EventManager *upnp.EventManager
	server       *http.Server // Per-renderer HTTP server
	cancel       context.CancelFunc
	ssdpCancel   context.CancelFunc // Cancels SSDP announcements for this renderer
}

// StateBroadcaster is called when renderer state changes.
type StateBroadcaster func(rendererID, transportState string, running bool)

// VolumeBroadcaster is called when renderer volume changes from external source.
type VolumeBroadcaster func(rendererID string, volume int, muted bool)

// Bridge is a unified DLNA bridge with multiple embedded renderers.
type Bridge struct {
	db        *database.DB
	disco     *discovery.Service
	localIP   string
	port      int
	startTime time.Time
	server    *http.Server

	renderers      map[string]*RendererInstance // keyed by renderer ID (UUID)
	stoppedByUser  map[string]bool              // renderers stopped from the UI: never auto-recovered
	castAdvertiser *cast.Advertiser             // Chromecast mDNS advertiser
	castServers    map[string]*cast.Server      // CASTV2 servers per renderer
	plugins        *plugins.Registry            // Volume control plugins
	mu             sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc

	// Callback for broadcasting state changes to web UI
	onStateChange  StateBroadcaster
	onVolumeChange VolumeBroadcaster
}

// NewBridge creates a new unified bridge.
func NewBridge(db *database.DB, disco *discovery.Service, port int) *Bridge {
	return &Bridge{
		db:             db,
		disco:          disco,
		port:           port,
		renderers:      make(map[string]*RendererInstance),
		stoppedByUser:  make(map[string]bool),
		castAdvertiser: cast.NewAdvertiser(),
		castServers:    make(map[string]*cast.Server),
	}
}

// SetStateBroadcaster sets the callback for state changes.
func (b *Bridge) SetStateBroadcaster(cb StateBroadcaster) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onStateChange = cb
}

// SetVolumeBroadcaster sets the callback for volume changes.
func (b *Bridge) SetVolumeBroadcaster(cb VolumeBroadcaster) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onVolumeChange = cb
}

// SetPluginRegistry sets the plugin registry for volume control.
func (b *Bridge) SetPluginRegistry(registry *plugins.Registry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.plugins = registry
}

// Start initializes and starts the bridge.
func (b *Bridge) Start(ctx context.Context) error {
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.startTime = time.Now()

	// Get local IP
	b.localIP = getLocalIP()
	if b.localIP == "" {
		return fmt.Errorf("could not determine local IP address")
	}

	// Load enabled renderers from database
	if err := b.loadRenderers(); err != nil {
		return fmt.Errorf("load renderers: %w", err)
	}

	// Start HTTP server
	if err := b.startHTTPServer(); err != nil {
		return fmt.Errorf("start HTTP server: %w", err)
	}

	// A receiver that was off during the initial discovery window stays
	// "Standby" forever otherwise: keep retrying in the background.
	go b.rendererRecoveryLoop()

	return nil
}

// Stop shuts down the bridge.
func (b *Bridge) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Stop all renderers
	for _, r := range b.renderers {
		b.stopRenderer(r)
	}

	// Stop all Chromecast advertisements
	b.castAdvertiser.StopAll()

	// Shutdown HTTP server
	if b.server != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = b.server.Shutdown(shutdownCtx)
	}

	if b.cancel != nil {
		b.cancel()
	}
}

// loadRenderers loads enabled renderers from the database.
func (b *Bridge) loadRenderers() error {
	renderers, err := b.db.ListRenderers()
	if err != nil {
		return err
	}

	for _, r := range renderers {
		if r.Enabled {
			if err := b.addRenderer(&r); err != nil {
				log.Printf("Failed to load renderer %s: %v", r.Name, err)
			}
			// Enable Chromecast receiver if configured
			if r.CastEnabled {
				if err := b.EnableCastReceiver(r.ID, r.CastPort); err != nil {
					log.Printf("Failed to enable Cast receiver for %s: %v", r.Name, err)
				}
			}
		}
	}

	return nil
}

// addRenderer adds a renderer instance.
func (b *Bridge) addRenderer(r *database.Renderer) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Check if already exists
	if _, exists := b.renderers[r.ID]; exists {
		return nil
	}

	// Create renderer context
	ctx, cancel := context.WithCancel(b.ctx)

	inst := &RendererInstance{
		ID:           r.ID,
		Name:         r.Name,
		DeviceType:   r.DeviceType,
		AirPlayName:  r.AirPlayName,
		DeviceName:   r.DeviceName,
		Port:         r.Port,
		State:        state.New(ctx),
		EventManager: upnp.NewEventManager(),
		cancel:       cancel,
	}

	// Set up state change callback for WebSocket broadcast
	rendererID := r.ID
	if b.onStateChange != nil {
		cb := b.onStateChange
		inst.State.SetOnStateChange(func(s state.TransportState) {
			cb(rendererID, string(s), true)
		})
	}

	// Get device and create appropriate player based on device type
	deviceType := r.DeviceType
	if deviceType == "" {
		deviceType = "airplay" // Default to AirPlay for legacy renderers
	}

	// Find the device (unified lookup)
	deviceID := r.DeviceID
	if deviceID == "" {
		deviceID = r.AirPlayDeviceID // Fallback for legacy
	}
	device := b.disco.GetDevice(deviceID)
	if device == nil {
		cancel()
		return fmt.Errorf("device not found: %s", deviceID)
	}

	inst.Device = device
	inst.DeviceName = device.Name
	inst.AirPlayName = device.Name

	switch deviceType {
	case "chromecast":
		inst.Player = player.NewChromecastPlayer(device)
		log.Printf("Added Chromecast renderer: %s -> %s:%d", r.Name, device.Host, device.Port)
	default: // "airplay"
		// Sink selection. RAOP (AirPlay 1) stays the default; HomePod OS 27
		// silently drops third-party AirPlay 1 audio, so those setups run the
		// AirPlay 2 flow instead. Selected per deployment with AIRBRIDGE_SINK.
		var vol volumeConfigurable
		if useAirPlay2(device) {
			if ap2, err := player.NewAirPlay2Player(device); err != nil {
				log.Printf("Warning: AirPlay 2 sink unavailable for %s (%v); falling back to RAOP", r.Name, err)
			} else {
				inst.Player = ap2
				vol = ap2
				// Receiver-side commands (Siri, HomePod touch) must be mirrored
				// into DLNA state, otherwise the control point keeps showing
				// PLAYING after the speaker was paused by hand.
				ap2.SetRemoteCommandHandler(func(event string) {
					switch event {
					case "paused":
						inst.State.SetTransportState(state.StatePaused)
						inst.EventManager.NotifyTransportState(string(state.StatePaused))
						log.Printf("Receiver-side pause on %s (Siri/touch) -> PAUSED", r.Name)
					case "playing":
						inst.State.SetTransportState(state.StatePlaying)
						inst.EventManager.NotifyTransportState(string(state.StatePlaying))
						log.Printf("Receiver-side resume on %s -> PLAYING", r.Name)
					case "stopped":
						inst.State.SetTransportState(state.StateStopped)
						inst.EventManager.NotifyTransportState(string(state.StateStopped))
						log.Printf("Receiver-side stop on %s -> STOPPED", r.Name)
					case "next", "previous":
						log.Printf("Receiver-side %s on %s ignored (no playlist)", event, r.Name)
					}
				})
				log.Printf("Added AirPlay 2 (native) renderer: %s -> %s:%d", r.Name, device.Host, device.Port)
			}
		}
		if inst.Player == nil {
			rp := player.NewRAOPPlayer(device)
			inst.Player = rp
			vol = rp
			log.Printf("Added AirPlay renderer: %s -> %s:%d", r.Name, device.Host, device.Port)
		}

		// Configure volume control from plugin if set
		if r.VolumePluginID != "" && b.plugins != nil {
			if pluginInstance, ok := b.plugins.GetInstance(r.VolumePluginID); ok {
				deviceID := r.VolumeDeviceID
				vol.SetVolumePluginCallback(func(volume int) error {
					return pluginInstance.SetVolume(deviceID, volume)
				})
				log.Printf("Configured volume plugin for %s: plugin=%s, device=%s", r.Name, r.VolumePluginID, deviceID)
			} else {
				log.Printf("Warning: volume plugin %s not found for renderer %s", r.VolumePluginID, r.Name)
			}
		} else if r.VolumeURL != "" {
			// Legacy volume passthrough (backward compatibility)
			vol.SetVolumePassthrough(&player.VolumePassthrough{
				URL:      r.VolumeURL,
				Method:   r.VolumeMethod,
				Body:     r.VolumeBody,
				AuthUser: r.VolumeAuthUser,
				AuthPass: r.VolumeAuthPass,
			})
			log.Printf("Configured legacy volume passthrough for %s: %s", r.Name, r.VolumeURL)
		}
	}

	b.renderers[r.ID] = inst

	// Start per-renderer HTTP server
	b.startRendererHTTPServer(inst)

	// Start SSDP announcements for this renderer (using renderer's port)
	b.startRendererSSDP(inst)

	return nil
}

// startRendererHTTPServer starts a dedicated HTTP server for a renderer.
func (b *Bridge) startRendererHTTPServer(r *RendererInstance) {
	baseURL := fmt.Sprintf("http://%s:%d", b.localIP, r.Port)
	mux := http.NewServeMux()
	httpserver.RegisterHTTP(mux, baseURL, r.ID, r.Name, "Airbridge", r.State, r.Player, r.EventManager)

	logged := httpserver.LogMiddleware(mux)
	r.server = &http.Server{
		Addr: fmt.Sprintf(":%d", r.Port),
		// Track control traffic so the idle watchdog can tell "the app is
		// gone" from "the app is just not talking right now".
		Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			logged.ServeHTTP(w, req)
		}),
	}

	go func() {
		log.Printf("Starting renderer HTTP server: %s on port %d", r.Name, r.Port)
		if err := r.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error for %s: %v", r.Name, err)
		}
	}()
}

// startRendererSSDP starts SSDP announcements for a specific renderer.
func (b *Bridge) startRendererSSDP(r *RendererInstance) {
	// Use the renderer's own port for SSDP Location
	baseURL := fmt.Sprintf("http://%s:%d", b.localIP, r.Port)
	serverName := "Airbridge/1.0"
	uuid := "uuid:" + r.ID

	ssdpCtx, ssdpCancel := context.WithCancel(b.ctx)
	r.ssdpCancel = ssdpCancel

	// Use simple /device.xml path since each renderer has its own HTTP server
	go ssdp.Announce(ssdpCtx, baseURL, uuid, serverName)
	go ssdp.SearchResponder(ssdpCtx, baseURL, uuid, serverName)
	log.Printf("Started SSDP announcements for renderer: %s on port %d", r.Name, r.Port)
}

// RemoveRenderer removes a renderer instance.
func (b *Bridge) RemoveRenderer(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if r, exists := b.renderers[id]; exists {
		b.stopRenderer(r)
		delete(b.renderers, id)
		log.Printf("Removed renderer: %s", r.Name)
	}
}

// stopRenderer stops a single renderer.
func (b *Bridge) stopRenderer(r *RendererInstance) {
	// Stop SSDP announcements first
	if r.ssdpCancel != nil {
		r.ssdpCancel()
		log.Printf("Stopped SSDP announcements for renderer: %s", r.Name)
	}
	// Shutdown per-renderer HTTP server
	if r.server != nil {
		if err := r.server.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down HTTP server for %s: %v", r.Name, err)
		}
		log.Printf("Stopped HTTP server for renderer: %s on port %d", r.Name, r.Port)
	}
	if r.cancel != nil {
		r.cancel()
	}
	if r.State != nil {
		r.State.Stop()
	}
}

// GetRenderer returns a renderer by ID.
func (b *Bridge) GetRenderer(id string) *RendererInstance {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.renderers[id]
}

// GetRenderers returns all active renderers.
func (b *Bridge) GetRenderers() []*RendererInstance {
	b.mu.RLock()
	defer b.mu.RUnlock()

	result := make([]*RendererInstance, 0, len(b.renderers))
	for _, r := range b.renderers {
		result = append(result, r)
	}
	return result
}

// IsRunning checks if a renderer is active.
func (b *Bridge) IsRunning(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, exists := b.renderers[id]
	return exists
}

// RunningCount returns the number of active renderers.
func (b *Bridge) RunningCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.renderers)
}

// Uptime returns how long the bridge has been running.
func (b *Bridge) Uptime() time.Duration {
	return time.Since(b.startTime)
}

// LocalIP returns the local IP address.
func (b *Bridge) LocalIP() string {
	return b.localIP
}

// GetTransportState returns the transport state for a renderer ("PLAYING", "STOPPED", etc).
func (b *Bridge) GetTransportState(id string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if r, ok := b.renderers[id]; ok && r.State != nil {
		return string(r.State.GetTransportState())
	}
	return ""
}

// GetVolume returns the current volume (0-100) for a renderer, or -1 if not running.
func (b *Bridge) GetVolume(id string) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if r, ok := b.renderers[id]; ok && r.State != nil {
		return r.State.GetVolume()
	}
	return -1
}

// SetVolume sets the volume for a renderer.
func (b *Bridge) SetVolume(id string, volume int) error {
	b.mu.RLock()
	r, ok := b.renderers[id]
	b.mu.RUnlock()

	if !ok || r.State == nil {
		return fmt.Errorf("renderer not running: %s", id)
	}

	r.State.SetVolume(volume)
	if r.Player != nil {
		if err := r.Player.SetVolume(context.Background(), volume); err != nil {
			log.Printf("SetVolume error for %s: %v", id, err)
		}
	}
	if r.EventManager != nil {
		r.EventManager.NotifyVolume(volume, r.State.GetMute())
	}
	return nil
}

// UpdateVolumeFromPlugin handles volume updates from external plugin webhooks.
// It finds renderers using the specified plugin/device and updates their volume state.
func (b *Bridge) UpdateVolumeFromPlugin(pluginID, deviceID string, volume int, muted bool) {
	// Look up renderers from database that use this plugin/device combo
	renderers, err := b.db.ListRenderers()
	if err != nil {
		log.Printf("UpdateVolumeFromPlugin: failed to list renderers: %v", err)
		return
	}

	for _, dbRenderer := range renderers {
		if dbRenderer.VolumePluginID == pluginID && dbRenderer.VolumeDeviceID == deviceID {
			b.mu.RLock()
			inst, ok := b.renderers[dbRenderer.ID]
			volumeCb := b.onVolumeChange
			b.mu.RUnlock()

			if ok && inst.State != nil && inst.EventManager != nil {
				log.Printf("Updating volume from plugin: renderer=%s, volume=%d, muted=%v", dbRenderer.Name, volume, muted)
				inst.State.SetVolumeInitial(volume)
				inst.State.SetMute(muted)
				inst.EventManager.NotifyVolume(volume, muted)

				// Broadcast to web UI
				if volumeCb != nil {
					volumeCb(dbRenderer.ID, volume, muted)
				}
			}
		}
	}
}

// GetRenderersByPlugin returns renderer IDs that use a specific plugin.
func (b *Bridge) GetRenderersByPlugin(pluginID string) []string {
	renderers, err := b.db.ListRenderers()
	if err != nil {
		return nil
	}

	var ids []string
	for _, r := range renderers {
		if r.VolumePluginID == pluginID && r.Enabled {
			ids = append(ids, r.ID)
		}
	}
	return ids
}

// StartRenderer starts a renderer by ID.
func (b *Bridge) StartRenderer(id string) error {
	b.mu.Lock()
	delete(b.stoppedByUser, id)
	b.mu.Unlock()

	renderer, err := b.db.GetRenderer(id)
	if err != nil {
		return err
	}
	if renderer == nil {
		return fmt.Errorf("renderer not found: %s", id)
	}
	return b.addRenderer(renderer)
}

// StopRenderer stops a renderer by ID.
func (b *Bridge) StopRenderer(id string) {
	b.mu.Lock()
	b.stoppedByUser[id] = true
	b.mu.Unlock()
	b.RemoveRenderer(id)
}

// RestartAll stops and restarts all enabled renderers.
func (b *Bridge) RestartAll() {
	log.Println("Restarting all renderers...")

	// Stop all
	b.mu.Lock()
	for _, r := range b.renderers {
		b.stopRenderer(r)
	}
	b.renderers = make(map[string]*RendererInstance)
	b.mu.Unlock()

	// Reload from database
	if err := b.loadRenderers(); err != nil {
		log.Printf("Failed to reload renderers: %v", err)
	}

	log.Println("All renderers restarted")
}

// StopAll stops all running renderers without reloading.
func (b *Bridge) StopAll() {
	log.Println("Stopping all renderers...")

	b.mu.Lock()
	defer b.mu.Unlock()

	for _, r := range b.renderers {
		b.stopRenderer(r)
	}
	b.renderers = make(map[string]*RendererInstance)

	// Stop all Chromecast advertisements
	b.castAdvertiser.StopAll()

	log.Println("All renderers stopped")
}

// EnableCastReceiver enables Chromecast receiver advertisement for a renderer.
// The port is the CASTV2 protocol port (typically 8009).
func (b *Bridge) EnableCastReceiver(rendererID string, port int) error {
	b.mu.Lock()
	r, ok := b.renderers[rendererID]
	if !ok {
		b.mu.Unlock()
		return fmt.Errorf("renderer not found: %s", rendererID)
	}

	// Stop existing server if any
	if srv, exists := b.castServers[rendererID]; exists {
		srv.Stop()
		delete(b.castServers, rendererID)
	}

	// Create and start CASTV2 server
	srv := cast.NewServer(port, r.Name, rendererID)
	srv.OnMediaLoad = func(url, contentType string) {
		log.Printf("Cast media load: %s (%s)", url, contentType)
		if r.Player != nil {
			ctx := context.Background()
			volume := r.State.GetStartVolume()
			if err := r.Player.Play(ctx, url, volume); err != nil {
				log.Printf("Cast Play error: %v", err)
			}
		}
	}

	if err := srv.Start(); err != nil {
		b.mu.Unlock()
		return fmt.Errorf("start cast server: %w", err)
	}
	b.castServers[rendererID] = srv
	b.mu.Unlock()

	// Advertise via mDNS
	cfg := cast.DeviceConfig{
		UUID:         rendererID,
		FriendlyName: r.Name,
		Model:        "Airbridge",
		Port:         port,
	}

	return b.castAdvertiser.Advertise(cfg)
}

// DisableCastReceiver disables Chromecast receiver advertisement for a renderer.
func (b *Bridge) DisableCastReceiver(rendererID string) {
	b.castAdvertiser.Stop(rendererID)

	b.mu.Lock()
	if srv, exists := b.castServers[rendererID]; exists {
		srv.Stop()
		delete(b.castServers, rendererID)
	}
	b.mu.Unlock()
}

// IsCastReceiverEnabled returns whether Chromecast receiver is enabled for a renderer.
func (b *Bridge) IsCastReceiverEnabled(rendererID string) bool {
	return b.castAdvertiser.IsAdvertising(rendererID)
}

// startHTTPServer starts the unified HTTP server.
func (b *Bridge) startHTTPServer() error {
	mux := http.NewServeMux()

	// Root device description (shows all embedded devices)
	mux.HandleFunc("/device.xml", b.handleDeviceDescription)

	// Service descriptions (shared) - kept for legacy embedded device support
	mux.HandleFunc("/upnp/service/avtransport.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=1800")
		_, _ = w.Write([]byte(upnp.SCPDAVTransportXML()))
	})
	mux.HandleFunc("/upnp/service/renderingcontrol.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=1800")
		_, _ = w.Write([]byte(upnp.SCPDRenderingControlXML()))
	})
	mux.HandleFunc("/upnp/service/connectionmanager.xml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=1800")
		_, _ = w.Write([]byte(upnp.SCPDConnectionManagerXML()))
	})

	b.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", b.port),
		Handler: logMiddleware(mux),
	}

	go func() {
		log.Printf("Bridge HTTP server listening on port %d", b.port)
		if err := b.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	return nil
}

// handleDeviceDescription returns the root device XML with embedded renderers.
func (b *Bridge) handleDeviceDescription(w http.ResponseWriter, r *http.Request) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	baseURL := fmt.Sprintf("http://%s:%d", b.localIP, b.port)
	xml := b.generateDeviceXML(baseURL)

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=1800")
	_, _ = w.Write([]byte(xml))
}

// generateDeviceXML creates the root device description with embedded devices.
func (b *Bridge) generateDeviceXML(baseURL string) string {
	var embeddedDevices strings.Builder

	// Sort renderers by name for consistent ordering
	renderers := make([]*RendererInstance, 0, len(b.renderers))
	for _, r := range b.renderers {
		renderers = append(renderers, r)
	}
	sort.Slice(renderers, func(i, j int) bool {
		return renderers[i].Name < renderers[j].Name
	})

	for _, r := range renderers {
		embeddedDevices.WriteString(fmt.Sprintf(`
    <device>
      <deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>
      <friendlyName>%s</friendlyName>
      <manufacturer>Airbridge</manufacturer>
      <modelName>Airbridge Renderer</modelName>
      <modelDescription>DLNA to AirPlay Bridge - %s</modelDescription>
      <modelNumber>1.0</modelNumber>
      <UDN>uuid:%s</UDN>
      <serviceList>
        <service>
          <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
          <serviceId>urn:upnp-org:serviceId:AVTransport</serviceId>
          <SCPDURL>/upnp/service/avtransport.xml</SCPDURL>
          <controlURL>/renderer/%s/control/avtransport</controlURL>
          <eventSubURL>/renderer/%s/event/avtransport</eventSubURL>
        </service>
        <service>
          <serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>
          <serviceId>urn:upnp-org:serviceId:RenderingControl</serviceId>
          <SCPDURL>/upnp/service/renderingcontrol.xml</SCPDURL>
          <controlURL>/renderer/%s/control/renderingcontrol</controlURL>
          <eventSubURL>/renderer/%s/event/renderingcontrol</eventSubURL>
        </service>
        <service>
          <serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>
          <serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId>
          <SCPDURL>/upnp/service/connectionmanager.xml</SCPDURL>
          <controlURL>/renderer/%s/control/connectionmanager</controlURL>
          <eventSubURL>/renderer/%s/event/connectionmanager</eventSubURL>
        </service>
      </serviceList>
    </device>`, r.Name, r.AirPlayName, r.ID, r.ID, r.ID, r.ID, r.ID, r.ID, r.ID))
	}

	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion>
    <major>1</major>
    <minor>0</minor>
  </specVersion>
  <URLBase>%s</URLBase>
  <device>
    <deviceType>urn:schemas-upnp-org:device:Basic:1</deviceType>
    <friendlyName>Airbridge</friendlyName>
    <manufacturer>Airbridge</manufacturer>
    <modelName>Airbridge Hub</modelName>
    <modelDescription>DLNA to AirPlay Bridge</modelDescription>
    <modelNumber>1.0</modelNumber>
    <UDN>uuid:%s</UDN>
    <deviceList>%s
    </deviceList>
  </device>
</root>`, baseURL, rootUUID, embeddedDevices.String())
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("HTTP %s %s from=%s duration=%v", r.Method, r.URL.Path, r.RemoteAddr, time.Since(start))
	})
}

func getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}

// volumeConfigurable is implemented by every AirPlay sink that can forward
// volume changes to a plugin or an external API.
type volumeConfigurable interface {
	SetVolumePluginCallback(cb func(volume int) error)
	SetVolumePassthrough(vp *player.VolumePassthrough)
}

// useAirPlay2 decides which AirPlay flow to use for a device.
//
// AIRBRIDGE_SINK:
//   - ""/"raop"  : AirPlay 1 / RAOP (default, widest compatibility)
//   - "ap2"      : AirPlay 2 native for every AirPlay device
//   - "auto"     : AirPlay 2 for Apple audio devices (model AudioAccessory*),
//     RAOP for everything else. HomePod OS 27 needs this.
func useAirPlay2(device *discovery.Device) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("AIRBRIDGE_SINK"))) {
	case "ap2", "airplay2", "ap2-native", "native":
		return true
	case "auto":
		return strings.HasPrefix(device.Model, "AudioAccessory")
	}
	return false
}

// rendererRecoveryLoop loads enabled renderers that have no live instance once
// their target device becomes discoverable again. Renderers are otherwise only
// loaded at startup, so a HomePod that was unplugged (or on another network)
// during that window stayed in Standby until someone clicked Start.
func (b *Bridge) rendererRecoveryLoop() {
	const interval = 20 * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastFailure := make(map[string]time.Time)

	for {
		select {
		case <-b.ctx.Done():
			return
		case <-ticker.C:
			renderers, err := b.db.ListRenderers()
			if err != nil {
				continue
			}
			for _, r := range renderers {
				if !r.Enabled || b.IsRunning(r.ID) || b.isStoppedByUser(r.ID) {
					continue
				}

				deviceID := r.DeviceID
				if deviceID == "" {
					deviceID = r.AirPlayDeviceID
				}
				if b.disco.GetDevice(deviceID) == nil {
					continue
				}

				if err := b.StartRenderer(r.ID); err != nil {
					if time.Since(lastFailure[r.ID]) > 5*time.Minute {
						log.Printf("Recovery of renderer %s failed: %v", r.Name, err)
						lastFailure[r.ID] = time.Now()
					}
					continue
				}
				log.Printf("Recovered renderer %s: its device is discoverable again", r.Name)

				if r.CastEnabled {
					if err := b.EnableCastReceiver(r.ID, r.CastPort); err != nil {
						log.Printf("Recovery: cast receiver for %s: %v", r.Name, err)
					}
				}
			}
		}
	}
}

// isStoppedByUser reports whether a renderer was explicitly stopped from the
// UI, in which case the recovery loop must leave it alone.
func (b *Bridge) isStoppedByUser(id string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.stoppedByUser[id]
}
