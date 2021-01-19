package server

import (
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/gogo/protobuf/types"
	"golang.org/x/net/context"

	"github.com/pachyderm/pachyderm/src/client/auth"
	lc "github.com/pachyderm/pachyderm/src/client/license"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/keycache"
	"github.com/pachyderm/pachyderm/src/server/pkg/license"
	"github.com/pachyderm/pachyderm/src/server/pkg/log"
	"github.com/pachyderm/pachyderm/src/server/pkg/serviceenv"
)

const (
	// enterpriseTokenKey is the constant key we use that maps to an Enterprise
	// token that a user has given us. This is what we check to know if a
	// Pachyderm cluster supports enterprise features
	enterpriseTokenKey = "token"

	licensePrefix = "/license"
)

type apiServer struct {
	pachLogger log.Logger
	env        *serviceenv.ServiceEnv

	enterpriseTokenCache *keycache.Cache

	// enterpriseToken is a collection containing at most one Pachyderm enterprise
	// token
	enterpriseToken col.Collection
}

func (a *apiServer) LogReq(request interface{}) {
	a.pachLogger.Log(request, nil, nil, 0)
}

// NewEnterpriseServer returns an implementation of lc.APIServer.
func NewEnterpriseServer(env *serviceenv.ServiceEnv, etcdPrefix string) (lc.APIServer, error) {
	defaultRecord := &lc.LicenseRecord{}
	enterpriseToken := col.NewCollection(
		env.GetEtcdClient(),
		etcdPrefix+licensePrefix,
		nil,
		&lc.LicenseRecord{},
		nil,
		nil,
	)

	s := &apiServer{
		pachLogger:           log.NewLogger("license.API"),
		env:                  env,
		enterpriseTokenCache: keycache.NewCache(enterpriseToken, enterpriseTokenKey, defaultRecord),
		enterpriseToken:      enterpriseToken,
	}
	go s.enterpriseTokenCache.Watch()
	return s, nil
}

// Activate implements the Activate RPC
func (a *apiServer) Activate(ctx context.Context, req *lc.ActivateRequest) (resp *lc.ActivateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	// Validate the activation code
	expiration, err := license.Validate(req.ActivationCode)
	if err != nil {
		return nil, errors.Wrapf(err, "error validating activation code")
	}
	// Allow request to override expiration in the activation code, for testing
	if req.Expires != nil {
		customExpiration, err := types.TimestampFromProto(req.Expires)
		if err == nil && expiration.After(customExpiration) {
			expiration = customExpiration
		}
	}
	expirationProto, err := types.TimestampProto(expiration)
	if err != nil {
		return nil, errors.Wrapf(err, "could not convert expiration time \"%s\" to proto", expiration.String())
	}
	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		e := a.enterpriseToken.ReadWrite(stm)
		// blind write
		return e.Put(enterpriseTokenKey, &lc.LicenseRecord{
			ActivationCode: req.ActivationCode,
			Expires:        expirationProto,
		})
	}); err != nil {
		return nil, err
	}

	// Wait until watcher observes the write
	if err := backoff.Retry(func() error {
		record, ok := a.enterpriseTokenCache.Load().(*lc.LicenseRecord)
		if !ok {
			return errors.Errorf("could not retrieve enterprise expiration time")
		}
		expiration, err := types.TimestampFromProto(record.Expires)
		if err != nil {
			return errors.Wrapf(err, "could not parse expiration timestamp")
		}
		if expiration.IsZero() {
			return errors.Errorf("enterprise not activated")
		}
		return nil
	}, backoff.RetryEvery(time.Second)); err != nil {
		return nil, err
	}
	time.Sleep(time.Second) // give other pachd nodes time to observe the write

	return &lc.ActivateResponse{
		Info: &lc.TokenInfo{
			Expires: expirationProto,
		},
	}, nil
}

// GetActivationCode returns the current state of the cluster's Pachyderm Enterprise key (ACTIVE, EXPIRED, or NONE), including the enterprise activation code
func (a *apiServer) GetActivationCode(ctx context.Context, req *lc.GetActivationCodeRequest) (resp *lc.GetActivationCodeResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	pachClient := a.env.GetPachClient(ctx)
	whoAmI, err := pachClient.WhoAmI(pachClient.Ctx(), &auth.WhoAmIRequest{})
	if err != nil {
		if !auth.IsErrNotActivated(err) {
			return nil, err
		}
	} else {
		if !whoAmI.IsAdmin {
			return nil, &auth.ErrNotAuthorized{
				Subject: whoAmI.Username,
				AdminOp: "GetActivationCode",
			}
		}
	}

	return a.getLicenseRecord()
}

func (a *apiServer) getLicenseRecord() (*lc.GetActivationCodeResponse, error) {
	record, ok := a.enterpriseTokenCache.Load().(*lc.LicenseRecord)
	if !ok {
		return nil, errors.Errorf("could not retrieve enterprise expiration time")
	}
	expiration, err := types.TimestampFromProto(record.Expires)
	if err != nil {
		return nil, errors.Wrapf(err, "could not parse expiration timestamp")
	}
	if expiration.IsZero() {
		return &lc.GetActivationCodeResponse{State: lc.State_NONE}, nil
	}
	resp := &lc.GetActivationCodeResponse{
		Info: &lc.TokenInfo{
			Expires: record.Expires,
		},
		ActivationCode: record.ActivationCode,
	}
	if time.Now().After(expiration) {
		resp.State = lc.State_EXPIRED
	} else {
		resp.State = lc.State_ACTIVE
	}
	return resp, nil
}

// Deactivate deletes the current enterprise license token, disabling the license service.
func (a *apiServer) Deactivate(ctx context.Context, req *lc.DeactivateRequest) (resp *lc.DeactivateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	if _, err := col.NewSTM(ctx, a.env.GetEtcdClient(), func(stm col.STM) error {
		err := a.enterpriseToken.ReadWrite(stm).Delete(enterpriseTokenKey)
		if err != nil && !col.IsErrNotFound(err) {
			return err
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// Wait until watcher observes the write
	if err := backoff.Retry(func() error {
		record, ok := a.enterpriseTokenCache.Load().(*lc.LicenseRecord)
		if !ok {
			return errors.Errorf("could not retrieve enterprise expiration time")
		}
		expiration, err := types.TimestampFromProto(record.Expires)
		if err != nil {
			return errors.Wrapf(err, "could not parse expiration timestamp")
		}
		if !expiration.IsZero() {
			return errors.Errorf("enterprise still activated")
		}
		return nil
	}, backoff.RetryEvery(time.Second)); err != nil {
		return nil, err
	}
	time.Sleep(time.Second) // give other pachd nodes time to observe the write

	return &lc.DeactivateResponse{}, nil
}

func (a *apiServer) AddCluster(ctx context.Context, req *lc.AddClusterRequest) (resp *lc.AddClusterResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, nil, retErr, time.Since(start)) }(time.Now())

	return &lc.AddClusterResponse{}, nil
}

func (a *apiServer) DeleteCluster(ctx context.Context, req *lc.DeleteClusterRequest) (resp *lc.DeleteClusterResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	return &lc.DeleteClusterResponse{}, nil
}

func (a *apiServer) Heartbeat(ctx context.Context, req *lc.HeartbeatRequest) (resp *lc.HeartbeatResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	return &lc.HeartbeatResponse{}, nil
}