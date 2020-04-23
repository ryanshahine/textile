package core

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/dgrijalva/jwt-go"
	grpcm "github.com/grpc-ecosystem/go-grpc-middleware"
	auth "github.com/grpc-ecosystem/go-grpc-middleware/auth"
	"github.com/improbable-eng/grpc-web/go/grpcweb"
	httpapi "github.com/ipfs/go-ipfs-http-client"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/peer"
	ma "github.com/multiformats/go-multiaddr"
	filc "github.com/textileio/filecoin/api/client"
	dbapi "github.com/textileio/go-threads/api"
	threads "github.com/textileio/go-threads/api/client"
	dbpb "github.com/textileio/go-threads/api/pb"
	"github.com/textileio/go-threads/broadcast"
	tc "github.com/textileio/go-threads/common"
	"github.com/textileio/go-threads/core/thread"
	netapi "github.com/textileio/go-threads/net/api"
	netpb "github.com/textileio/go-threads/net/api/pb"
	tutil "github.com/textileio/go-threads/util"
	"github.com/textileio/textile/api/buckets"
	bpb "github.com/textileio/textile/api/buckets/pb"
	"github.com/textileio/textile/api/common"
	"github.com/textileio/textile/api/hub"
	hpb "github.com/textileio/textile/api/hub/pb"
	"github.com/textileio/textile/api/users"
	upb "github.com/textileio/textile/api/users/pb"
	c "github.com/textileio/textile/collections"
	"github.com/textileio/textile/dns"
	"github.com/textileio/textile/email"
	"github.com/textileio/textile/gateway"
	"github.com/textileio/textile/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	log = logging.Logger("core")

	ignoreMethods = []string{
		"/hub.pb.API/Signup",
		"/hub.pb.API/Signin",
		"/hub.pb.API/IsUsernameAvailable",
	}
)

type Textile struct {
	co *c.Collections

	ts tc.NetBoostrapper
	th *threads.Client

	fc *filc.Client

	server *grpc.Server
	proxy  *http.Server

	gateway        *gateway.Gateway
	gatewaySession string
	sessionBus     *broadcast.Broadcaster
}

type Config struct {
	RepoPath string

	AddrApi                 ma.Multiaddr
	AddrApiProxy            ma.Multiaddr
	AddrThreadsHost         ma.Multiaddr
	AddrIpfsApi             ma.Multiaddr
	AddrGatewayHost         ma.Multiaddr
	AddrGatewayUrl          string
	AddrGatewayBucketDomain string
	AddrFilecoinApi         ma.Multiaddr
	AddrMongoUri            string

	MongoName string

	DNSDomain string
	DNSZoneID string
	DNSToken  string

	EmailFrom   string
	EmailDomain string
	EmailApiKey string

	SessionSecret string

	Debug bool
}

func NewTextile(ctx context.Context, conf Config) (*Textile, error) {
	if conf.Debug {
		if err := tutil.SetLogLevels(map[string]logging.LogLevel{
			"core":    logging.LevelDebug,
			"hub":     logging.LevelDebug,
			"buckets": logging.LevelDebug,
			"users":   logging.LevelDebug,
		}); err != nil {
			return nil, err
		}
	}
	t := &Textile{}

	// Configure clients
	ic, err := httpapi.NewApi(conf.AddrIpfsApi)
	if err != nil {
		return nil, err
	}
	if conf.AddrFilecoinApi != nil {
		t.fc, err = filc.NewClient(conf.AddrFilecoinApi)
		if err != nil {
			return nil, err
		}
	}
	var dnsm *dns.Manager
	if conf.DNSToken != "" {
		dnsm, err = dns.NewManager(conf.DNSDomain, conf.DNSZoneID, conf.DNSToken, conf.Debug)
		if err != nil {
			return nil, err
		}
	}
	ec, err := email.NewClient(conf.EmailFrom, conf.EmailDomain, conf.EmailApiKey, conf.Debug)
	if err != nil {
		return nil, err
	}
	t.co, err = c.NewCollections(ctx, conf.AddrMongoUri, conf.MongoName)
	if err != nil {
		return nil, err
	}

	// Configure threads
	t.ts, err = tc.DefaultNetwork(conf.RepoPath, tc.WithNetHostAddr(conf.AddrThreadsHost), tc.WithNetDebug(conf.Debug))
	if err != nil {
		return nil, err
	}

	// Configure gRPC services
	ts, err := dbapi.NewService(t.ts, dbapi.Config{
		RepoPath: conf.RepoPath,
		Debug:    conf.Debug,
	})
	if err != nil {
		return nil, err
	}
	ns, err := netapi.NewService(t.ts, netapi.Config{
		Debug: conf.Debug,
	})
	if err != nil {
		return nil, err
	}
	t.sessionBus = broadcast.NewBroadcaster(0)
	hs := &hub.Service{
		Collections:   t.co,
		EmailClient:   ec,
		GatewayUrl:    conf.AddrGatewayUrl,
		SessionBus:    t.sessionBus,
		SessionSecret: conf.SessionSecret,
	}
	bucks := &buckets.Buckets{}
	bs := &buckets.Service{
		Buckets:        bucks,
		IPFSClient:     ic,
		FilecoinClient: t.fc,
		GatewayUrl:     conf.AddrGatewayUrl,
		DNSManager:     dnsm,
	}
	us := &users.Service{
		Collections: t.co,
	}

	// Configure gRPC server
	target, err := tutil.TCPAddrFromMultiAddr(conf.AddrApi)
	if err != nil {
		return nil, err
	}
	ptarget, err := tutil.TCPAddrFromMultiAddr(conf.AddrApiProxy)
	if err != nil {
		return nil, err
	}
	t.server = grpc.NewServer(
		grpcm.WithUnaryServerChain(auth.UnaryServerInterceptor(t.authFunc), t.threadInterceptor()),
		grpcm.WithStreamServerChain(auth.StreamServerInterceptor(t.authFunc)))
	listener, err := net.Listen("tcp", target)
	if err != nil {
		return nil, err
	}
	go func() {
		dbpb.RegisterAPIServer(t.server, ts)
		netpb.RegisterAPIServer(t.server, ns)
		hpb.RegisterAPIServer(t.server, hs)
		bpb.RegisterAPIServer(t.server, bs)
		upb.RegisterAPIServer(t.server, us)
		if err := t.server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			log.Fatalf("serve error: %v", err)
		}
	}()
	webrpc := grpcweb.WrapServer(
		t.server,
		grpcweb.WithOriginFunc(func(origin string) bool {
			return true
		}),
		grpcweb.WithWebsockets(true),
		grpcweb.WithWebsocketOriginFunc(func(req *http.Request) bool {
			return true
		}))
	t.proxy = &http.Server{
		Addr: ptarget,
	}
	t.proxy.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if webrpc.IsGrpcWebRequest(r) ||
			webrpc.IsAcceptableGrpcCorsRequest(r) ||
			webrpc.IsGrpcWebSocketRequest(r) {
			webrpc.ServeHTTP(w, r)
		}
	})
	go func() {
		if err := t.proxy.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("proxy error: %v", err)
		}
	}()

	// Start threads client
	t.th, err = threads.NewClient(target, grpc.WithInsecure(), grpc.WithPerRPCCredentials(common.Credentials{}))
	if err != nil {
		return nil, err
	}
	bucks.Threads = t.th
	hs.Threads = t.th

	// Configure gateway
	t.gatewaySession = util.MakeToken(32)
	t.gateway, err = gateway.NewGateway(
		conf.AddrGatewayHost,
		conf.AddrApi,
		t.gatewaySession,
		conf.AddrGatewayBucketDomain,
		t.co,
		t.sessionBus,
		conf.Debug)
	if err != nil {
		return nil, err
	}
	t.gateway.Start()

	log.Info("started")

	return t, nil
}

func (t *Textile) Bootstrap() {
	t.ts.Bootstrap(tutil.DefaultBoostrapPeers())
}

func (t *Textile) Close() error {
	t.sessionBus.Discard()
	if err := t.gateway.Stop(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := t.proxy.Shutdown(ctx); err != nil {
		return err
	}
	t.server.GracefulStop()
	if err := t.th.Close(); err != nil {
		return err
	}
	if err := t.ts.Close(); err != nil {
		return err
	}
	if t.fc != nil {
		if err := t.fc.Close(); err != nil {
			return err
		}
	}
	if err := t.co.Close(); err != nil {
		return err
	}
	return nil
}

func (t *Textile) HostID() peer.ID {
	return t.ts.Host().ID()
}

func getService(method string) string {
	return strings.Split(method, "/")[1]
}

func (t *Textile) authFunc(ctx context.Context) (context.Context, error) {
	method, _ := grpc.Method(ctx)
	for _, ignored := range ignoreMethods {
		if method == ignored {
			return ctx, nil
		}
	}

	threadID, ok := common.ThreadIDFromMD(ctx)
	if ok {
		ctx = common.NewThreadIDContext(ctx, threadID)
	}
	threadName, ok := common.ThreadNameFromMD(ctx)
	if ok {
		ctx = common.NewThreadNameContext(ctx, threadName)
	}
	threadToken, err := thread.NewTokenFromMD(ctx)
	if err != nil {
		return nil, err
	}
	ctx = thread.NewTokenContext(ctx, threadToken)

	sid, ok := common.SessionFromMD(ctx)
	if ok {
		ctx = common.NewSessionContext(ctx, sid) // For additional requests
		if sid == t.gatewaySession {
			return ctx, nil
		}
		session, err := t.co.Sessions.Get(ctx, sid)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "Invalid session")
		}
		if time.Now().After(session.ExpiresAt) {
			return nil, status.Error(codes.Unauthenticated, "Expired session")
		}
		if err := t.co.Sessions.Touch(ctx, session.ID); err != nil {
			return nil, err
		}
		ctx = c.NewSessionContext(ctx, session)

		dev, err := t.co.Accounts.Get(ctx, session.Owner)
		if err != nil {
			return nil, status.Error(codes.NotFound, "User not found")
		}
		ctx = c.NewDevContext(ctx, dev)

		orgSlug, ok := common.OrgSlugFromMD(ctx)
		if ok {
			isMember, err := t.co.Accounts.IsMember(ctx, orgSlug, dev.Key)
			if err != nil {
				return nil, err
			}
			if !isMember {
				return nil, status.Error(codes.PermissionDenied, "User is not an org member")
			} else {
				org, err := t.co.Accounts.GetByUsername(ctx, orgSlug)
				if err != nil {
					return nil, status.Error(codes.NotFound, "Org not found")
				}
				ctx = c.NewOrgContext(ctx, org)
				ctx = common.NewOrgSlugContext(ctx, orgSlug) // For additional requests
				ctx = thread.NewTokenContext(ctx, org.Token)
			}
		} else {
			ctx = thread.NewTokenContext(ctx, dev.Token)
		}
	} else if key, ok := common.APIKeyFromMD(ctx); ok {
		ctx = common.NewAPIKeyContext(ctx, key)
		msg, sig, ok := common.APISigFromMD(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "API key signature required")
		}
		ctx = common.NewAPISigContext(ctx, msg, sig)
	} else {
		return nil, status.Error(codes.Unauthenticated, "Session or API key required")
	}
	return ctx, nil
}

// threadInterceptor monitors for thread creation and deletion.
// Textile tracks threads against dev, org, and user accounts.
// Users must supply a valid API key from a dev/org.
func (t *Textile) threadInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		method, _ := grpc.Method(ctx)
		var isUser bool
		var owner crypto.PubKey
		if org, ok := c.OrgFromContext(ctx); ok {
			owner = org.Key
		} else if dev, ok := c.DevFromContext(ctx); ok {
			owner = dev.Key
		} else if k, ok := common.APIKeyFromContext(ctx); ok {
			key, err := t.co.Keys.Get(ctx, k)
			if err != nil || !key.Valid {
				return nil, status.Error(codes.NotFound, "API key not found or is invalid")
			}
			if !common.ValidateAPISigContext(ctx, key.Secret) {
				return nil, status.Error(codes.PermissionDenied, "Bad API key signature")
			}

			// Pull user key out of the thread token if present
			token, ok := thread.TokenFromContext(ctx)
			if ok {
				var claims jwt.StandardClaims
				if _, _, err = new(jwt.Parser).ParseUnverified(string(token), &claims); err != nil {
					return nil, status.Error(codes.PermissionDenied, "Bad authorization")
				}
				ukey := &thread.Libp2pPubKey{}
				if err = ukey.UnmarshalString(claims.Subject); err != nil {
					return nil, err
				}
				owner = ukey.PubKey
				isUser = true
			}
			if getService(method) == "users.pb.API" {
				if owner == nil {
					return nil, status.Error(codes.Unauthenticated, "Token required")
				}
				user, err := t.co.Users.Get(ctx, owner)
				if err != nil {
					return nil, status.Error(codes.NotFound, "User not found")
				}
				ctx = c.NewUserContext(ctx, user)
			}
		}

		// Let the request pass through
		res, err := handler(ctx, req)
		if err != nil {
			return res, err
		}

		var createID, deleteID thread.ID
		switch method {
		case "/threads.pb.API/NewDB":
			createID, err = thread.Cast(req.(*dbpb.NewDBRequest).DbID)
			if err != nil {
				return res, err
			}
		case "/threads.pb.API/NewDBFromAddr":
			addr, err := ma.NewMultiaddrBytes(req.(*dbpb.NewDBFromAddrRequest).Addr)
			if err != nil {
				return res, err
			}
			createID, err = thread.FromAddr(addr)
			if err != nil {
				return res, err
			}
		case "/threads.pb.API/DeleteDB":
			deleteID, err = thread.Cast(req.(*dbpb.DeleteDBRequest).DbID)
			if err != nil {
				return res, err
			}
		case "/threads.net.pb.API/CreateThread":
			createID, err = thread.Cast(req.(*netpb.CreateThreadRequest).ThreadID)
			if err != nil {
				return res, err
			}
		case "/threads.net.pb.API/AddThread":
			addr, err := ma.NewMultiaddrBytes(req.(*netpb.AddThreadRequest).Addr)
			if err != nil {
				return res, err
			}
			createID, err = thread.FromAddr(addr)
			if err != nil {
				return res, err
			}
		case "/threads.net.pb.API/DeleteThread":
			deleteID, err = thread.Cast(req.(*netpb.DeleteThreadRequest).ThreadID)
			if err != nil {
				return res, err
			}
		default:
			return res, nil
		}

		if createID.Defined() {
			if isUser {
				if err := t.co.Users.Create(ctx, owner); err != nil {
					return nil, err
				}
			}
			if _, err := t.co.Threads.Create(ctx, createID, owner); err != nil {
				return nil, err
			}
		} else if deleteID.Defined() {
			if err := t.co.Threads.Delete(ctx, deleteID, owner); err != nil {
				return nil, err
			}
		}
		return res, nil
	}
}