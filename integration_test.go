package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/nspcc-dev/neo-go/pkg/crypto/keys"
	restv1 "github.com/nspcc-dev/neofs-http-gw/rest/v1/handlers"
	"github.com/nspcc-dev/neofs-http-gw/rest/v1/model"
	"github.com/nspcc-dev/neofs-sdk-go/container"
	cid "github.com/nspcc-dev/neofs-sdk-go/container/id"
	"github.com/nspcc-dev/neofs-sdk-go/eacl"
	"github.com/nspcc-dev/neofs-sdk-go/object"
	"github.com/nspcc-dev/neofs-sdk-go/object/address"
	oid "github.com/nspcc-dev/neofs-sdk-go/object/id"
	"github.com/nspcc-dev/neofs-sdk-go/policy"
	"github.com/nspcc-dev/neofs-sdk-go/pool"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

type putResponse struct {
	CID string `json:"container_id"`
	OID string `json:"object_id"`
}

const (
	devenvPrivateKey = "1dd37fba80fec4e6a6f13fd708d8dcb3b29def768017052f6c930fa1c5d90bbb"
	testHost         = "http://localhost:8082"
)

func TestIntegration(t *testing.T) {
	rootCtx := context.Background()
	aioImage := "nspccdev/neofs-aio-testcontainer:"
	versions := []string{"0.24.0", "0.25.1", "0.26.1", "0.27.0", "latest"}
	key, err := keys.NewPrivateKeyFromHex(devenvPrivateKey)
	require.NoError(t, err)

	for _, version := range versions {
		ctx, cancel2 := context.WithCancel(rootCtx)

		aioContainer := createDockerContainer(ctx, t, aioImage+version)
		cancel := runServer()
		clientPool := getPool(ctx, t, key)
		CID, err := createContainer(ctx, t, clientPool)
		require.NoError(t, err, version)

		t.Run("simple put "+version, func(t *testing.T) { simplePut(ctx, t, clientPool, CID) })
		t.Run("simple get "+version, func(t *testing.T) { simpleGet(ctx, t, clientPool, CID) })
		t.Run("get by attribute "+version, func(t *testing.T) { getByAttr(ctx, t, clientPool, CID) })
		t.Run("get zip "+version, func(t *testing.T) { getZip(ctx, t, clientPool, CID) })

		t.Run("rest put "+version, func(t *testing.T) { restObjectPut(ctx, t, clientPool, CID) })

		cancel()
		err = aioContainer.Terminate(ctx)
		require.NoError(t, err)
		cancel2()
	}
}

func runServer() context.CancelFunc {
	cancelCtx, cancel := context.WithCancel(context.Background())

	v := getDefaultConfig()
	l := newLogger(v)
	application := newApp(cancelCtx, WithConfig(v), WithLogger(l))
	go application.Serve(cancelCtx)

	return cancel
}

func simplePut(ctx context.Context, t *testing.T, clientPool *pool.Pool, CID *cid.ID) {
	content := "content of file"
	keyAttr, valAttr := "User-Attribute", "user value"

	attributes := map[string]string{
		object.AttributeFileName: "newFile.txt",
		keyAttr:                  valAttr,
	}

	var buff bytes.Buffer
	w := multipart.NewWriter(&buff)
	fw, err := w.CreateFormFile("file", attributes[object.AttributeFileName])
	require.NoError(t, err)
	_, err = io.Copy(fw, bytes.NewBufferString(content))
	require.NoError(t, err)
	err = w.Close()
	require.NoError(t, err)

	request, err := http.NewRequest(http.MethodPost, "http://localhost:8082/upload/"+CID.String(), &buff)
	require.NoError(t, err)
	request.Header.Set("Content-Type", w.FormDataContentType())
	request.Header.Set("X-Attribute-"+keyAttr, valAttr)

	resp, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() {
		err = resp.Body.Close()
		require.NoError(t, err)
	}()

	addr := &putResponse{}
	err = json.NewDecoder(resp.Body).Decode(addr)
	require.NoError(t, err)

	err = CID.Parse(addr.CID)
	require.NoError(t, err)

	id := oid.NewID()
	err = id.Parse(addr.OID)
	require.NoError(t, err)

	objectAddress := address.NewAddress()
	objectAddress.SetContainerID(CID)
	objectAddress.SetObjectID(id)

	payload := bytes.NewBuffer(nil)

	res, err := clientPool.GetObject(ctx, *objectAddress)
	require.NoError(t, err)

	_, err = io.Copy(payload, res.Payload)
	require.NoError(t, err)

	require.Equal(t, content, payload.String())

	for _, attribute := range res.Header.Attributes() {
		require.Equal(t, attributes[attribute.Key()], attribute.Value())
	}
}

func simpleGet(ctx context.Context, t *testing.T, clientPool *pool.Pool, CID *cid.ID) {
	content := "content of file"
	attributes := map[string]string{
		"some-attr": "some-get-value",
	}

	id := putObject(ctx, t, clientPool, CID, content, attributes)

	resp, err := http.Get("http://localhost:8082/get/" + CID.String() + "/" + id.String())
	require.NoError(t, err)
	defer func() {
		err = resp.Body.Close()
		require.NoError(t, err)
	}()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, content, string(data))

	for k, v := range attributes {
		require.Equal(t, v, resp.Header.Get("X-Attribute-"+k))
	}
}

func getByAttr(ctx context.Context, t *testing.T, clientPool *pool.Pool, CID *cid.ID) {
	keyAttr, valAttr := "some-attr", "some-get-by-attr-value"
	content := "content of file"
	attributes := map[string]string{keyAttr: valAttr}

	id := putObject(ctx, t, clientPool, CID, content, attributes)

	expectedAttr := map[string]string{
		"X-Attribute-" + keyAttr: valAttr,
		"x-object-id":            id.String(),
		"x-container-id":         CID.String(),
	}

	resp, err := http.Get("http://localhost:8082/get_by_attribute/" + CID.String() + "/" + keyAttr + "/" + valAttr)
	require.NoError(t, err)
	defer func() {
		err = resp.Body.Close()
		require.NoError(t, err)
	}()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, content, string(data))

	for k, v := range expectedAttr {
		require.Equal(t, v, resp.Header.Get(k))
	}
}

func getZip(ctx context.Context, t *testing.T, clientPool *pool.Pool, CID *cid.ID) {
	names := []string{"zipfolder/dir/name1.txt", "zipfolder/name2.txt"}
	contents := []string{"content of file1", "content of file2"}
	attributes1 := map[string]string{object.AttributeFileName: names[0]}
	attributes2 := map[string]string{object.AttributeFileName: names[1]}

	putObject(ctx, t, clientPool, CID, contents[0], attributes1)
	putObject(ctx, t, clientPool, CID, contents[1], attributes2)

	resp, err := http.Get("http://localhost:8082/zip/" + CID.String() + "/zipfolder")
	require.NoError(t, err)
	defer func() {
		err = resp.Body.Close()
		require.NoError(t, err)
	}()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	checkZip(t, data, resp.ContentLength, names, contents)

	// check nested folder
	resp2, err := http.Get("http://localhost:8082/zip/" + CID.String() + "/zipfolder/dir")
	require.NoError(t, err)
	defer func() {
		err = resp2.Body.Close()
		require.NoError(t, err)
	}()

	data2, err := io.ReadAll(resp2.Body)
	require.NoError(t, err)
	checkZip(t, data2, resp2.ContentLength, names[:1], contents[:1])
}

func checkZip(t *testing.T, data []byte, length int64, names, contents []string) {
	readerAt := bytes.NewReader(data)

	zipReader, err := zip.NewReader(readerAt, length)
	require.NoError(t, err)

	require.Equal(t, len(names), len(zipReader.File))

	sort.Slice(zipReader.File, func(i, j int) bool {
		return zipReader.File[i].FileHeader.Name < zipReader.File[j].FileHeader.Name
	})

	for i, f := range zipReader.File {
		require.Equal(t, names[i], f.FileHeader.Name)

		rc, err := f.Open()
		require.NoError(t, err)

		all, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.Equal(t, contents[i], string(all))

		err = rc.Close()
		require.NoError(t, err)
	}
}

func restObjectPut(ctx context.Context, t *testing.T, clientPool *pool.Pool, cnrID *cid.ID) {
	restrictByEACL(ctx, t, clientPool, cnrID)

	key, err := keys.NewPrivateKeyFromHex(devenvPrivateKey)
	require.NoError(t, err)

	b := model.Bearer{
		Records: []model.Record{{
			Operation: model.OperationPut,
			Action:    model.ActionAllow,
			Filters:   []model.Filter{},
			Targets: []model.Target{{
				Role: model.RoleOthers,
				Keys: []string{},
			}},
		}},
	}

	data, err := json.Marshal(&b)
	require.NoError(t, err)

	request0, err := http.NewRequest(http.MethodPost, testHost+"/v1/auth", bytes.NewReader(data))
	require.NoError(t, err)
	request0.Header.Add("Content-Type", "application/json")

	httpClient := http.Client{
		Timeout: 5 * time.Second,
	}

	resp, err := httpClient.Do(request0)
	require.NoError(t, err)
	defer resp.Body.Close()

	rr, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	bearerBase64 := string(rr)
	fmt.Println(bearerBase64)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	binaryData, err := base64.StdEncoding.DecodeString(bearerBase64)
	require.NoError(t, err)

	h := sha512.Sum512(binaryData)
	x, y, err := ecdsa.Sign(rand.Reader, &key.PrivateKey, h[:])
	if err != nil {
		panic(err)
	}
	signatureData := elliptic.Marshal(elliptic.P256(), x, y)

	content := "content of file"
	attrKey, attrValue := "User-Attribute", "user value"

	attributes := map[string]string{
		object.AttributeFileName: "newFile.txt",
		attrKey:                  attrValue,
	}

	req := model.ObjectsPutRequest{
		ContainerID: cnrID.String(),
		FileName:    "newFile.txt",
		Payload:     base64.StdEncoding.EncodeToString([]byte(content)),
	}

	body, err := json.Marshal(&req)
	require.NoError(t, err)

	fmt.Println(base64.StdEncoding.EncodeToString(signatureData))
	fmt.Println(hex.EncodeToString(key.PublicKey().Bytes()))

	request, err := http.NewRequest(http.MethodPut, testHost+"/v1/objects", bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Add("Content-Type", "application/json")
	request.Header.Add(restv1.XNeofsBearerSignature, base64.StdEncoding.EncodeToString(signatureData))
	request.Header.Add("Authorization", "Bearer "+bearerBase64)
	request.Header.Add(restv1.XNeofsBearerOwnerKey, hex.EncodeToString(key.PublicKey().Bytes()))
	request.Header.Add("X-Attribute-"+attrKey, attrValue)

	resp2, err := httpClient.Do(request)
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	addr := &model.ObjectsPutResponse{}
	err = json.NewDecoder(resp2.Body).Decode(addr)
	require.NoError(t, err)

	var CID cid.ID
	err = CID.Parse(addr.ContainerID)
	require.NoError(t, err)

	id := oid.NewID()
	err = id.Parse(addr.ObjectID)
	require.NoError(t, err)

	objectAddress := address.NewAddress()
	objectAddress.SetContainerID(&CID)
	objectAddress.SetObjectID(id)

	payload := bytes.NewBuffer(nil)

	res, err := clientPool.GetObject(ctx, *objectAddress)
	require.NoError(t, err)

	_, err = io.Copy(payload, res.Payload)
	require.NoError(t, err)

	require.Equal(t, content, payload.String())

	for _, attribute := range res.Header.Attributes() {
		require.Equal(t, attributes[attribute.Key()], attribute.Value(), attribute.Key())
	}
}

func createDockerContainer(ctx context.Context, t *testing.T, image string) testcontainers.Container {
	req := testcontainers.ContainerRequest{
		Image:       image,
		WaitingFor:  wait.NewLogStrategy("aio container started").WithStartupTimeout(30 * time.Second),
		Name:        "aio",
		Hostname:    "aio",
		NetworkMode: "host",
	}
	aioC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	require.NoError(t, err)

	return aioC
}

func getDefaultConfig() *viper.Viper {
	v := settings()
	v.SetDefault(cfgPeers+".0.address", "127.0.0.1:8080")
	v.SetDefault(cfgPeers+".0.weight", 1)
	v.SetDefault(cfgPeers+".0.priority", 1)

	return v
}

func getPool(ctx context.Context, t *testing.T, key *keys.PrivateKey) *pool.Pool {
	pb := new(pool.Builder)
	pb.AddNode("localhost:8080", 1, 1)

	opts := &pool.BuilderOptions{
		Key:                   &key.PrivateKey,
		NodeConnectionTimeout: 5 * time.Second,
		NodeRequestTimeout:    5 * time.Second,
	}
	clientPool, err := pb.Build(ctx, opts)
	require.NoError(t, err)
	return clientPool
}

func createContainer(ctx context.Context, t *testing.T, clientPool *pool.Pool) (*cid.ID, error) {
	pp, err := policy.Parse("REP 1")
	require.NoError(t, err)

	cnr := container.New(
		container.WithPolicy(pp),
		container.WithCustomBasicACL(0x0FFFFFFF),
		container.WithAttribute(container.AttributeName, "friendlyName"),
		container.WithAttribute(container.AttributeTimestamp, strconv.FormatInt(time.Now().Unix(), 10)))
	cnr.SetOwnerID(clientPool.OwnerID())

	CID, err := clientPool.PutContainer(ctx, cnr)
	if err != nil {
		return nil, err
	}
	fmt.Println(CID.String())

	err = clientPool.WaitForContainerPresence(ctx, CID, &pool.ContainerPollingParams{
		CreationTimeout: 15 * time.Second,
		PollInterval:    3 * time.Second,
	})

	return CID, err
}

func restrictByEACL(ctx context.Context, t *testing.T, clientPool *pool.Pool, cnrID *cid.ID) {
	table := new(eacl.Table)
	table.SetCID(cnrID)

	for op := eacl.OperationGet; op <= eacl.OperationRangeHash; op++ {
		record := new(eacl.Record)
		record.SetOperation(op)
		record.SetAction(eacl.ActionDeny)
		target := new(eacl.Target)
		target.SetRole(eacl.RoleOthers)
		record.SetTargets(*target)
		table.AddRecord(record)
	}

	err := clientPool.SetEACL(ctx, table)
	require.NoError(t, err)

	prm := &waitParams{
		WaitTimeout:  15 * time.Second,
		PollInterval: 1 * time.Second,
	}

	err = waitEACLPresence(ctx, clientPool, cnrID, table, prm)
	require.NoError(t, err)
}

func waitEACLPresence(ctx context.Context, p *pool.Pool, cnrID *cid.ID, table *eacl.Table, params *waitParams) error {
	exp, err := table.Marshal()
	if err != nil {
		return fmt.Errorf("couldn't marshal eacl: %w", err)
	}

	return waitFor(ctx, params, func(ctx context.Context) bool {
		eaclTable, err := p.GetEACL(ctx, cnrID)
		if err == nil {
			got, err := eaclTable.Marshal()
			if err == nil && bytes.Equal(exp, got) {
				return true
			}
		}
		return false
	})
}

type waitParams struct {
	WaitTimeout  time.Duration
	PollInterval time.Duration
}

// waitFor await that given condition will be met in waitParams time.
func waitFor(ctx context.Context, params *waitParams, condition func(context.Context) bool) error {
	wctx, cancel := context.WithTimeout(ctx, params.WaitTimeout)
	defer cancel()
	ticker := time.NewTimer(params.PollInterval)
	defer ticker.Stop()
	wdone := wctx.Done()
	done := ctx.Done()
	for {
		select {
		case <-done:
			return ctx.Err()
		case <-wdone:
			return wctx.Err()
		case <-ticker.C:
			if condition(ctx) {
				return nil
			}
			ticker.Reset(params.PollInterval)
		}
	}
}

func putObject(ctx context.Context, t *testing.T, clientPool *pool.Pool, CID *cid.ID, content string, attributes map[string]string) *oid.ID {
	obj := object.New()
	obj.SetContainerID(CID)
	obj.SetOwnerID(clientPool.OwnerID())

	var attrs []object.Attribute
	for key, val := range attributes {
		attr := object.NewAttribute()
		attr.SetKey(key)
		attr.SetValue(val)
		attrs = append(attrs, *attr)
	}
	obj.SetAttributes(attrs...)

	id, err := clientPool.PutObject(ctx, *obj, bytes.NewBufferString(content))
	require.NoError(t, err)

	return id
}
