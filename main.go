package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v4"
)

type streamHandler struct {
	rtpChan        chan []byte
	processedChan  chan []byte
	done           chan struct{}
	ffmpegStdin    io.WriteCloser
	workerCount    int
	metricsEnabled bool
}

func newStreamHandler(workers int) *streamHandler {
	return &streamHandler{
		rtpChan:        make(chan []byte, 100), // Smaller buffer to reduce latency
		processedChan:  make(chan []byte, 100), // Processed packets ready for FFmpeg
		done:           make(chan struct{}),
		workerCount:    workers,
		metricsEnabled: true,
	}
}

func (h *streamHandler) processRTPPackets(track *webrtc.TrackRemote) {
	defer close(h.processedChan)

	packetCounter := uint64(0)
	lastMetricTime := time.Now()

	workers := make(chan struct{}, h.workerCount)

	for {
		select {
		case <-h.done:
			return
		default:
			rtpPacket, _, err := track.ReadRTP()
			if err != nil {
				fmt.Println("Error reading RTP:", err)
				return
			}

			select {
			case workers <- struct{}{}: // Acquire worker
				go func(packet []byte) {
					defer func() { <-workers }() // Release worker

					// Process packet in parallel
					payload := make([]byte, len(packet))
					copy(payload, packet)

					select {
					case h.processedChan <- payload:
						if h.metricsEnabled {
							packetCounter++
							if time.Since(lastMetricTime) >= time.Second {
								fmt.Printf("Processed %d packets/sec\n", packetCounter)
								packetCounter = 0
								lastMetricTime = time.Now()
							}
						}
					default:
						if h.metricsEnabled {
							fmt.Println("Packet dropped: buffer full")
						}
					}
				}(rtpPacket.Payload)
			default:
				if h.metricsEnabled {
					fmt.Println("Worker pool full, dropping packet")
				}
			}
		}
	}
}

func (h *streamHandler) writeToFFmpeg() {
	const batchSize = 5 // Process packets in small batches for efficiency
	batch := make([][]byte, 0, batchSize)

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}

		for _, payload := range batch {
			if _, err := h.ffmpegStdin.Write(payload); err != nil {
				fmt.Println("Error writing to FFmpeg:", err)
				return
			}
		}
		batch = batch[:0]
	}

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case payload, ok := <-h.processedChan:
			if !ok {
				flushBatch()
				return
			}

			batch = append(batch, payload)
			if len(batch) >= batchSize {
				flushBatch()
			}

		case <-ticker.C:
			flushBatch()
		}
	}
}

func (h *streamHandler) startFFmpeg() error {
	cmd := exec.Command(
		"ffmpeg",
		"-fflags", "+nobuffer+fastseek+flush_packets+discardcorrupt",
		"-flags", "low_delay",
		"-f", "opus",
		"-i", "pipe:0",
		"-c:a", "copy",
		"-f", "segment",
		"-segment_time", "0.025",
		"-segment_format", "ogg",
		"-segment_list_flags", "+live",
		"-segment_list_size", "2",
		"-segment_list", "stream.m3u8",
		"-segment_format_options", "flush_packets=1",
		"-max_delay", "0",
		"-avoid_negative_ts", "make_zero",
		"-segment_list_type", "m3u8",
		"-thread_queue_size", "512",
		"-segment_filename", "stream_%d.ogg",
	)

	var err error
	h.ffmpegStdin, err = cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %v", err)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Start()
}

func saveToDisk(writer io.Writer, track *webrtc.TrackRemote) {
	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			fmt.Println("Error reading RTP:", err)
			return
		}

		if _, err := writer.Write(rtpPacket.Payload); err != nil {
			fmt.Println("Error writing payload:", err)
			return
		}
	}
}

// nolint:gocognit
func main() {
	// Everything below is the Pion WebRTC API! Thanks for using it .

	// Create a MediaEngine object to configure the supported codec
	m := &webrtc.MediaEngine{}

	// Setup the codecs you want to use.
	// We'll use a VP8 and Opus but you can also define your own
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 0, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	}

	// Create a InterceptorRegistry. This is the user configurable RTP/RTCP Pipeline.
	// This provides NACKs, RTCP Reports and other features. If you use `webrtc.NewPeerConnection`
	// this is enabled by default. If you are manually managing You MUST create a InterceptorRegistry
	// for each PeerConnection.
	i := &interceptor.Registry{}

	// Register a intervalpli factory
	// This interceptor sends a PLI every 3 seconds. A PLI causes a video keyframe to be generated by the sender.
	// This makes our video seekable and more error resilent, but at a cost of lower picture quality and higher bitrates
	// A real world application should process incoming RTCP packets from viewers and forward them to senders
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		panic(err)
	}
	i.Add(intervalPliFactory)

	// Use the default set of Interceptors
	if err = webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new RTCPeerConnection
	peerConnection, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	// Allow us to receive 1 audio track, and 1 video track
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	} else if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	// Set a handler for when a new remote track starts
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		codec := track.Codec()
		if strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus) {
			fmt.Println("Got Opus track, starting ultra-low-latency stream")

			handler := newStreamHandler(4) // Use 4 workers for parallel processing

			if err := handler.startFFmpeg(); err != nil {
				fmt.Println("Failed to start FFmpeg:", err)
				return
			}

			// Start parallel processing pipeline
			go handler.processRTPPackets(track)
			go handler.writeToFFmpeg()

			// Create a done channel for cleanup
			done := make(chan struct{})
			go func() {
				<-done
				close(handler.done)
				handler.ffmpegStdin.Close()
			}()

			// Wait for peer connection to close
			peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
				if s == webrtc.PeerConnectionStateClosed {
					close(done)
				}
			})
		} else if strings.EqualFold(codec.MimeType, webrtc.MimeTypeVP8) {
			fmt.Println("Got VP8 track, streaming directly to FFmpeg")

			cmd := exec.Command(
				"ffmpeg",
				"-f", "rawvideo",
				"-pix_fmt", "yuv420p",
				"-s", "640x480",
				"-r", "30",
				"-i", "pipe:0",
				"-c:v", "libx264",
				"-preset", "veryfast",
				"-tune", "zerolatency",
				"-f", "segment",
				"-segment_time", "0.05",
				"-segment_format", "mp4",
				"-segment_list_flags", "+live",
				"-segment_list_size", "2",
				"-segment_list", "stream.m3u8",
				"-segment_format_options", "movflags=+frag_keyframe+empty_moov",
				"-max_delay", "0",
				"-avoid_negative_ts", "make_zero",
				"-segment_list_type", "m3u8",
				"-segment_filename", "stream_%d.mp4",
			)

			ffmpegStdin, err := cmd.StdinPipe()
			if err != nil {
				fmt.Println("Failed to create stdin pipe:", err)
				return
			}

			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr

			if err := cmd.Start(); err != nil {
				fmt.Println("Failed to start FFmpeg:", err)
				return
			}

			saveToDisk(ffmpegStdin, track)
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			fmt.Println("Ctrl+C the remote client to stop the demo")
		} else if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateClosed {
			fmt.Println("Done writing media files")

			// Gracefully shutdown the peer connection
			if closeErr := peerConnection.Close(); closeErr != nil {
				panic(closeErr)
			}

			os.Exit(0)
		}
	})

	// Wait for the offer to be pasted
	offer := webrtc.SessionDescription{}
	decode(readUntilNewline(), &offer)

	// Set the remote SessionDescription
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		panic(err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	// Output the answer in base64 so we can paste it in browser
	fmt.Println(encode(peerConnection.LocalDescription()))

	// Block forever
	select {}
}

// Read from stdin until we get a newline
func readUntilNewline() (in string) {
	var err error

	r := bufio.NewReader(os.Stdin)
	for {
		in, err = r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			panic(err)
		}

		if in = strings.TrimSpace(in); len(in) > 0 {
			break
		}
	}

	fmt.Println("")
	return
}

// JSON encode + base64 a SessionDescription
func encode(obj *webrtc.SessionDescription) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

// Decode a base64 and unmarshal JSON into a SessionDescription
func decode(in string, obj *webrtc.SessionDescription) {
	b, err := base64.StdEncoding.DecodeString(in)
	if err != nil {
		panic(err)
	}

	if err = json.Unmarshal(b, obj); err != nil {
		panic(err)
	}
}