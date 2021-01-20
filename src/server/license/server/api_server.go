package server

import (
	"time"

	"github.com/gogo/protobuf/types"
	"github.com/jmoiron/sqlx"
	"golang.org/x/net/context"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/auth"
	lc "github.com/pachyderm/pachyderm/src/client/license"
	"github.com/pachyderm/pachyderm/src/client/pkg/errors"
	"github.com/pachyderm/pachyderm/src/client/version"
	"github.com/pachyderm/pachyderm/src/server/pkg/backoff"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/keycache"
	"github.com/pachyderm/pachyderm/src/server/pkg/license"
	"github.com/pachyderm/pachyderm/src/server/pkg/log"
	"github.com/pachyderm/pachyderm/src/server/pkg/random"
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
	db         *sqlx.DB

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
		db:                   env.GetDBClient(),
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

func (a *apiServer) checkLicenseState() error {
	record, err := a.getLicenseRecord()
	if err != nil {
		return err
	}
	if record.State != lc.State_ACTIVE {
		return errors.New("enterprise license is not valid")
	}
	return nil
}

// Deactivate deletes the current enterprise license token, disabling the license service.
func (a *apiServer) Deactivate(ctx context.Context, req *lc.DeactivateRequest) (resp *lc.DeactivateResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, resp, retErr, time.Since(start)) }(time.Now())

	// Delete all cluster records
	if _, err := a.db.ExecContext(ctx, `DELETE FROM license.clusters`); err != nil {
		return nil, errors.Wrapf(err, "unable to delete clusters in table")
	}

	// Delete the license from etcd
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

// AddCluster registers a new pachd with this license server. Each pachd is configured with a shared secret
// which is used to authenticate to the license server when heartbeating.
func (a *apiServer) AddCluster(ctx context.Context, req *lc.AddClusterRequest) (resp *lc.AddClusterResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(req, nil, retErr, time.Since(start)) }(time.Now())

	// Make sure we have an active license
	if err := a.checkLicenseState(); err != nil {
		return nil, err
	}

	// Validate the request
	if req.Id == "" {
		return nil, errors.New("no id provided for cluster")
	}

	if req.Address == "" {
		return nil, errors.New("no address provided for cluster")
	}

	// Attempt to connect to the pachd
	pachClient, err := client.NewFromAddress(req.Address)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create client for %q", req.Address)
	}

	versionResp, err := pachClient.GetVersion(ctx, &types.Empty{})
	if err != nil {
		return nil, errors.Wrapf(err, "unable to connect to pachd at %q", req.Address)
	}

	// Generate the new shared secret for this pachd
	secret := random.String(30)

	// Register the pachd in the database
	if _, err := a.db.ExecContext(ctx, `INSERT INTO license.clusters (id, address, secret, version, enabled, auth_enabled) VALUES ($1, $2, $3, $4, %5, $6)`, req.Id, req.Address, secret, version.String(versionResp), true, false); err != nil {
		return nil, errors.Wrapf(err, "unable to register pachd in database")
	}

	return &lc.AddClusterResponse{
		Secret: secret,
	}, nil
}

func (a *apiServer) Heartbeat(ctx context.Context, req *lc.HeartbeatRequest) (resp *lc.HeartbeatResponse, retErr error) {
	a.LogReq(req)
	defer func(start time.Time) { a.pachLogger.Log(nil, resp, retErr, time.Since(start)) }(time.Now())

	var count int
	if err := a.db.GetContext(ctx, &count, `SELECT COUNT(*) FROM license.clusters WHERE id=$1 and secret=$2 and enabled`, req.Id, req.Secret); err != nil {
		return nil, errors.Wrapf(err, "unable to look up cluster in database")
	}

	if count != 1 {
		return nil, errors.New("invalid cluster id or secret")
	}

	if _, err := a.db.ExecContext(ctx, `UPDATE license.clusters SET version=$1 AND auth_enabled=$2 AND last_heartbeat=NOW()`, req.Version, req.AuthEnabled); err != nil {
		return nil, errors.Wrapf(err, "unable to update cluster in database")
	}

	record, ok := a.enterpriseTokenCache.Load().(*lc.LicenseRecord)
	if !ok {
		return nil, errors.New("unable to load current enterprise key")
	}

	return &lc.HeartbeatResponse{
		License: record,
	}, nil
}
