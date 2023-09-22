package uploader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nspcc-dev/neofs-http-gw/resolver"
	"github.com/nspcc-dev/neofs-http-gw/response"
	"github.com/nspcc-dev/neofs-http-gw/tokens"
	"github.com/nspcc-dev/neofs-http-gw/utils"
	"github.com/nspcc-dev/neofs-sdk-go/bearer"
	"github.com/nspcc-dev/neofs-sdk-go/client"
	"github.com/nspcc-dev/neofs-sdk-go/object"
	oid "github.com/nspcc-dev/neofs-sdk-go/object/id"
	"github.com/nspcc-dev/neofs-sdk-go/pool"
	"github.com/nspcc-dev/neofs-sdk-go/user"
	"github.com/valyala/fasthttp"
	"go.uber.org/zap"
)

const (
	jsonHeader   = "application/json; charset=UTF-8"
	drainBufSize = 4096
)

// Uploader is an upload request handler.
type Uploader struct {
	appCtx            context.Context
	log               *zap.Logger
	pool              *pool.Pool
	ownerID           *user.ID
	settings          *Settings
	containerResolver resolver.Resolver
	signer            user.Signer
}

type epochDurations struct {
	currentEpoch  uint64
	msPerBlock    int64
	blockPerEpoch uint64
}

// Settings stores reloading parameters, so it has to provide atomic getters and setters.
type Settings struct {
	defaultTimestamp atomic.Bool
	maxObjectSize    atomic.Int64
}

func (s *Settings) DefaultTimestamp() bool {
	return s.defaultTimestamp.Load()
}

func (s *Settings) SetDefaultTimestamp(val bool) {
	s.defaultTimestamp.Store(val)
}

func (s *Settings) SetMaxObjectSize(val int64) {
	s.maxObjectSize.Store(val)
}

// New creates a new Uploader using specified logger, connection pool and
// other options.
func New(ctx context.Context, params *utils.AppParams, settings *Settings, signer user.Signer) *Uploader {
	return &Uploader{
		appCtx:            ctx,
		log:               params.Logger,
		pool:              params.Pool,
		ownerID:           params.Owner,
		settings:          settings,
		containerResolver: params.Resolver,
		signer:            signer,
	}
}

// Upload handles multipart upload request.
func (u *Uploader) Upload(c *fasthttp.RequestCtx) {
	var (
		file       MultipartFile
		idObj      oid.ID
		addr       oid.Address
		scid, _    = c.UserValue("cid").(string)
		log        = u.log.With(zap.String("cid", scid))
		bodyStream = c.RequestBodyStream()
		drainBuf   = make([]byte, drainBufSize)
	)

	if err := tokens.StoreBearerToken(c); err != nil {
		log.Error("could not fetch bearer token", zap.Error(err))
		response.Error(c, "could not fetch bearer token", fasthttp.StatusBadRequest)
		return
	}

	idCnr, err := utils.GetContainerID(u.appCtx, scid, u.containerResolver)
	if err != nil {
		log.Error("wrong container id", zap.Error(err))
		response.Error(c, "wrong container id", fasthttp.StatusBadRequest)
		return
	}

	defer func() {
		// If the temporary reader can be closed - let's close it.
		if file == nil {
			return
		}
		err := file.Close()
		log.Debug(
			"close temporary multipart/form file",
			zap.Stringer("address", addr),
			zap.String("filename", file.FileName()),
			zap.Error(err),
		)
	}()
	boundary := string(c.Request.Header.MultipartFormBoundary())
	if file, err = fetchMultipartFile(u.log, bodyStream, boundary); err != nil {
		log.Error("could not receive multipart/form", zap.Error(err))
		response.Error(c, "could not receive multipart/form: "+err.Error(), fasthttp.StatusBadRequest)
		return
	}
	filtered, err := filterHeaders(u.log, &c.Request.Header)
	if err != nil {
		log.Error("could not process headers", zap.Error(err))
		response.Error(c, err.Error(), fasthttp.StatusBadRequest)
		return
	}
	if needParseExpiration(filtered) {
		epochDuration, err := getEpochDurations(c, u.pool)
		if err != nil {
			log.Error("could not get epoch durations from network info", zap.Error(err))
			response.Error(c, "could not get epoch durations from network info: "+err.Error(), fasthttp.StatusBadRequest)
			return
		}

		now := time.Now()
		if rawHeader := c.Request.Header.Peek(fasthttp.HeaderDate); rawHeader != nil {
			if parsed, err := time.Parse(http.TimeFormat, string(rawHeader)); err != nil {
				log.Warn("could not parse client time", zap.String("Date header", string(rawHeader)), zap.Error(err))
			} else {
				now = parsed
			}
		}

		if err = prepareExpirationHeader(filtered, epochDuration, now); err != nil {
			log.Error("could not parse expiration header", zap.Error(err))
			response.Error(c, "could not parse expiration header: "+err.Error(), fasthttp.StatusBadRequest)
			return
		}
	}

	attributes := make([]object.Attribute, 0, len(filtered))
	// prepares attributes from filtered headers
	for key, val := range filtered {
		attribute := object.NewAttribute()
		attribute.SetKey(key)
		attribute.SetValue(val)
		attributes = append(attributes, *attribute)
	}
	// sets FileName attribute if it wasn't set from header
	if _, ok := filtered[object.AttributeFileName]; !ok {
		filename := object.NewAttribute()
		filename.SetKey(object.AttributeFileName)
		filename.SetValue(file.FileName())
		attributes = append(attributes, *filename)
	}
	// sets Content-Type attribute if it wasn't set from header
	if _, ok := filtered[object.AttributeContentType]; !ok && file.ContentType() != "" {
		cType := object.NewAttribute()
		cType.SetKey(object.AttributeContentType)
		cType.SetValue(file.ContentType())
		attributes = append(attributes, *cType)
	}
	// sets Timestamp attribute if it wasn't set from header and enabled by settings
	if _, ok := filtered[object.AttributeTimestamp]; !ok && u.settings.DefaultTimestamp() {
		timestamp := object.NewAttribute()
		timestamp.SetKey(object.AttributeTimestamp)
		timestamp.SetValue(strconv.FormatInt(time.Now().Unix(), 10))
		attributes = append(attributes, *timestamp)
	}
	id, bt := u.fetchOwnerAndBearerToken(c)

	var obj object.Object
	obj.SetContainerID(*idCnr)
	obj.SetOwnerID(id)
	obj.SetAttributes(attributes...)

	var prm client.PrmObjectPutInit
	if bt != nil {
		prm.WithBearerToken(*bt)
	}

	writer, err := u.pool.ObjectPutInit(u.appCtx, obj, u.signer, prm)
	if err != nil {
		log.Error("writer init", zap.Error(err))
		response.Error(c, "writer init: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	chunk := make([]byte, u.settings.maxObjectSize.Load())
	_, err = io.CopyBuffer(writer, file, chunk)
	if err != nil {
		log.Error("write", zap.Error(err))
		response.Error(c, "write: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	if err = writer.Close(); err != nil {
		log.Error("close writer", zap.Error(err))
		response.Error(c, "close writer: "+err.Error(), fasthttp.StatusInternalServerError)
		return
	}

	idObj = writer.GetResult().StoredObjectID()

	addr.SetObject(idObj)
	addr.SetContainer(*idCnr)

	// Try to return the response, otherwise, if something went wrong, throw an error.
	if err = newPutResponse(addr).encode(c); err != nil {
		log.Error("could not encode response", zap.Error(err))
		response.Error(c, "could not encode response", fasthttp.StatusBadRequest)

		return
	}
	// Multipart is multipart and thus can contain more than one part which
	// we ignore at the moment. Also, when dealing with chunked encoding
	// the last zero-length chunk might be left unread (because multipart
	// reader only cares about its boundary and doesn't look further) and
	// it will be (erroneously) interpreted as the start of the next
	// pipelined header. Thus we need to drain the body buffer.
	for {
		_, err = bodyStream.Read(drainBuf)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
	}
	// Report status code and content type.
	c.Response.SetStatusCode(fasthttp.StatusOK)
	c.Response.Header.SetContentType(jsonHeader)
}

func (u *Uploader) fetchOwnerAndBearerToken(ctx context.Context) (*user.ID, *bearer.Token) {
	if tkn, err := tokens.LoadBearerToken(ctx); err == nil && tkn != nil {
		issuer := tkn.ResolveIssuer()
		return &issuer, tkn
	}
	return u.ownerID, nil
}

type putResponse struct {
	ObjectID    string `json:"object_id"`
	ContainerID string `json:"container_id"`
}

func newPutResponse(addr oid.Address) *putResponse {
	return &putResponse{
		ObjectID:    addr.Object().EncodeToString(),
		ContainerID: addr.Container().EncodeToString(),
	}
}

func (pr *putResponse) encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "\t")
	return enc.Encode(pr)
}

func getEpochDurations(ctx context.Context, p *pool.Pool) (*epochDurations, error) {
	networkInfo, err := p.NetworkInfo(ctx, client.PrmNetworkInfo{})
	if err != nil {
		return nil, err
	}

	res := &epochDurations{
		currentEpoch:  networkInfo.CurrentEpoch(),
		msPerBlock:    networkInfo.MsPerBlock(),
		blockPerEpoch: networkInfo.EpochDuration(),
	}

	if res.blockPerEpoch == 0 {
		return nil, fmt.Errorf("EpochDuration is empty")
	}
	return res, nil
}

func needParseExpiration(headers map[string]string) bool {
	_, ok1 := headers[utils.ExpirationDurationAttr]
	_, ok2 := headers[utils.ExpirationRFC3339Attr]
	_, ok3 := headers[utils.ExpirationTimestampAttr]
	return ok1 || ok2 || ok3
}
