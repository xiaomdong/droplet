package rpc

import (
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/ipfs-force-community/droplet/v2/piecestorage"
	logging "github.com/ipfs/go-log/v2"
)

var resourceLog = logging.Logger("resource")

var _ http.Handler = (*PieceStorageServer)(nil)

type PieceStorageServer struct {
	pieceStorageMgr *piecestorage.PieceStorageManager
}

func NewPieceStorageServer(pieceStorageMgr *piecestorage.PieceStorageManager) *PieceStorageServer {
	return &PieceStorageServer{pieceStorageMgr: pieceStorageMgr}
}

func (p *PieceStorageServer) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		p.handleGet(res, req)
	case http.MethodPut:
		p.handlePut(res, req)
	default:
		// handle error
		logErrorAndResonse(res, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
}

func (p *PieceStorageServer) handleGet(res http.ResponseWriter, req *http.Request) {
	resourceID := req.URL.Query().Get("resource-id")
	if len(resourceID) == 0 {
		logErrorAndResonse(res, "resource is empty", http.StatusBadRequest)
		return
	}
	ctx := req.Context()

	// todo consider priority strategy, priority oss, priority market transfer directly
	pieceStorage, err := p.pieceStorageMgr.FindStorageForRead(ctx, resourceID)
	if err != nil {
		logErrorAndResonse(res, fmt.Sprintf("resource %s not found", resourceID), http.StatusNotFound)
		return
	}

	redirectUrl, err := pieceStorage.GetRedirectUrl(ctx, resourceID)
	if err != nil && err != piecestorage.ErrUnsupportRedirect {
		logErrorAndResonse(res, fmt.Sprintf("fail to get redirect url of piece  %s: %s", resourceID, err), http.StatusInternalServerError)
		return
	}

	if err == nil {
		res.Header().Set("Location", redirectUrl)
		res.WriteHeader(http.StatusFound)
		return
	}

	flen, err := pieceStorage.Len(req.Context(), resourceID)
	if err != nil {
		logErrorAndResonse(res, fmt.Sprintf("call piecestore.Len for %s: %s", resourceID, err), http.StatusInternalServerError)
		return
	}
	res.Header().Set("Content-Length", strconv.FormatInt(flen, 10))

	r, err := pieceStorage.GetReaderCloser(req.Context(), resourceID)
	if err != nil {
		logErrorAndResonse(res, fmt.Sprintf("failed to open reader for %s: %s", resourceID, err), http.StatusInternalServerError)
		return
	}

	defer func() {
		if err = r.Close(); err != nil {
			log.Errorf("unable to close http %v", err)
		}
	}()

	// TODO:
	// as we can not override http response headers after body transfer has began
	// we can only log the error info here
	_, _ = io.Copy(res, r)
}

// handlePut save resource to piece storage
// url example: http://market/resource?resource-id=xxx&store=xxx or http://market/resource?resource-id=xxx&size=xxx
func (p *PieceStorageServer) handlePut(res http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	resourceID := req.URL.Query().Get("resource-id")
	if len(resourceID) == 0 {
		logErrorAndResonse(res, "resource is empty", http.StatusBadRequest)
		return
	}

	if req.Body == nil {
		logErrorAndResonse(res, "body is empty", http.StatusBadRequest)
		return
	}

	if !req.URL.Query().Has("store") && !req.URL.Query().Has("size") {
		logErrorAndResonse(res, "both store and size is empty", http.StatusBadRequest)
		return
	}

	var store piecestorage.IPieceStorage
	if req.URL.Query().Has("store") {
		storeName := req.URL.Query().Get("store")

		var err error
		store, err = p.pieceStorageMgr.GetPieceStorageByName(storeName)
		if err != nil {
			logErrorAndResonse(res, fmt.Sprintf("fail to get store %s: %s", storeName, err), http.StatusInternalServerError)
			return
		}
	}
	if store == nil && req.URL.Query().Has("size") {
		sizeStr := req.URL.Query().Get("size")
		size, err := strconv.ParseInt(sizeStr, 10, 64)
		if err != nil {
			logErrorAndResonse(res, fmt.Sprintf("size %s is invalid", sizeStr), http.StatusBadRequest)
			return
		}
		store, err = p.pieceStorageMgr.FindStorageForWrite(size)
		if err != nil {
			logErrorAndResonse(res, fmt.Sprintf("fail to find store for write: %s", err), http.StatusInternalServerError)
			return
		}
	}

	_, err := store.SaveTo(ctx, resourceID, req.Body)
	if err != nil {
		logErrorAndResonse(res, fmt.Sprintf("fail to save resource %s to store %s: %s", resourceID, store.GetName(), err), http.StatusInternalServerError)
		return
	}

	res.WriteHeader(http.StatusOK)
}

func logErrorAndResonse(res http.ResponseWriter, err string, code int) {
	resourceLog.Errorf("resource request fail Code: %d, Message: %s", code, err)
	http.Error(res, err, code)
}
