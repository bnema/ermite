// SPDX-License-Identifier: AGPL-3.0-or-later

package rpc

import (
	"context"

	"connectrpc.com/connect"

	pb "bnema.dev/ermite-api/gen/ermite"
	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// FeatureCISecrets is listed by GetVersion when the server serves the secret,
// variable and environment services and sends secrets, variables and the trust
// flag to runners.
const FeatureCISecrets = "ci.secrets"

// settingsScopeFromPB reads a SettingsScope. A scope that is not set, a false instance
// and a name that breaks the rules are invalid_argument.
func settingsScopeFromPB(s *pb.SettingsScope) (domain.SettingsScope, error) {
	bad := func(reason string) error {
		return &domain.InvalidFieldsError{Fields: []domain.FieldViolation{{Field: "scope", Reason: reason}}}
	}
	switch w := s.GetWhere().(type) {
	case *pb.SettingsScope_Instance:
		if !w.Instance {
			return domain.SettingsScope{}, bad("instance must be true when set")
		}
		return domain.InstanceScope(), nil
	case *pb.SettingsScope_Repo:
		name, err := domain.ParseRepoName(w.Repo)
		if err != nil {
			return domain.SettingsScope{}, err
		}
		return domain.RepoScope(name), nil
	case *pb.SettingsScope_Environment:
		name, err := domain.ParseRepoName(w.Environment.GetRepo())
		if err != nil {
			return domain.SettingsScope{}, err
		}
		env, err := domain.ParseEnvironmentName(w.Environment.GetName())
		if err != nil {
			return domain.SettingsScope{}, bad("environment name " + err.Error())
		}
		return domain.EnvironmentScope(name, env), nil
	}
	return domain.SettingsScope{}, bad("is required")
}

func secretToPB(s domain.Secret) *pb.Secret {
	return &pb.Secret{Name: s.Name, UpdatedAt: timestamp(s.UpdatedAt)} // never a value
}

func variableToPB(v domain.Variable) *pb.Variable {
	return &pb.Variable{Name: v.Name, Value: v.Value, UpdatedAt: timestamp(v.UpdatedAt)}
}

func environmentToPB(e domain.Environment) *pb.Environment {
	return &pb.Environment{Repo: e.Repo.String(), Name: e.Name, CreatedAt: timestamp(e.CreatedAt)}
}

// secretService implements ermiteconnect.SecretServiceHandler. A request holds
// a secret value: nothing of it is logged here, and the interceptor logs
// procedures only.
type secretService struct{ secrets ports.SecretService }

func (s *secretService) SetSecret(ctx context.Context, req *connect.Request[pb.SetSecretRequest]) (*connect.Response[pb.SetSecretResponse], error) {
	scope, err := settingsScopeFromPB(req.Msg.GetScope())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	sec, err := s.secrets.SetSecret(ctx, caller(ctx), scope, req.Msg.GetName(), req.Msg.GetValue())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.SetSecretResponse{Secret: secretToPB(sec)}), nil
}

func (s *secretService) ListSecrets(ctx context.Context, req *connect.Request[pb.ListSecretsRequest]) (*connect.Response[pb.ListSecretsResponse], error) {
	scope, err := settingsScopeFromPB(req.Msg.GetScope())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	page, err := s.secrets.ListSecrets(ctx, caller(ctx), scope, int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListSecretsResponse{NextPageToken: page.NextPageToken, Secrets: make([]*pb.Secret, 0, len(page.Secrets))}
	for _, x := range page.Secrets {
		out.Secrets = append(out.Secrets, secretToPB(x))
	}
	return connect.NewResponse(out), nil
}

func (s *secretService) DeleteSecret(ctx context.Context, req *connect.Request[pb.DeleteSecretRequest]) (*connect.Response[pb.DeleteSecretResponse], error) {
	scope, err := settingsScopeFromPB(req.Msg.GetScope())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if err := s.secrets.DeleteSecret(ctx, caller(ctx), scope, req.Msg.GetName()); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.DeleteSecretResponse{}), nil
}

// variableService implements ermiteconnect.VariableServiceHandler.
type variableService struct{ vars ports.VariableService }

func (s *variableService) SetVariable(ctx context.Context, req *connect.Request[pb.SetVariableRequest]) (*connect.Response[pb.SetVariableResponse], error) {
	scope, err := settingsScopeFromPB(req.Msg.GetScope())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	v, err := s.vars.SetVariable(ctx, caller(ctx), scope, req.Msg.GetName(), req.Msg.GetValue())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.SetVariableResponse{Variable: variableToPB(v)}), nil
}

func (s *variableService) GetVariable(ctx context.Context, req *connect.Request[pb.GetVariableRequest]) (*connect.Response[pb.GetVariableResponse], error) {
	scope, err := settingsScopeFromPB(req.Msg.GetScope())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	v, err := s.vars.GetVariable(ctx, caller(ctx), scope, req.Msg.GetName())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.GetVariableResponse{Variable: variableToPB(v)}), nil
}

func (s *variableService) ListVariables(ctx context.Context, req *connect.Request[pb.ListVariablesRequest]) (*connect.Response[pb.ListVariablesResponse], error) {
	scope, err := settingsScopeFromPB(req.Msg.GetScope())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	page, err := s.vars.ListVariables(ctx, caller(ctx), scope, int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListVariablesResponse{NextPageToken: page.NextPageToken, Variables: make([]*pb.Variable, 0, len(page.Variables))}
	for _, x := range page.Variables {
		out.Variables = append(out.Variables, variableToPB(x))
	}
	return connect.NewResponse(out), nil
}

func (s *variableService) DeleteVariable(ctx context.Context, req *connect.Request[pb.DeleteVariableRequest]) (*connect.Response[pb.DeleteVariableResponse], error) {
	scope, err := settingsScopeFromPB(req.Msg.GetScope())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if err := s.vars.DeleteVariable(ctx, caller(ctx), scope, req.Msg.GetName()); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.DeleteVariableResponse{}), nil
}

// environmentService implements ermiteconnect.EnvironmentServiceHandler.
type environmentService struct{ envs ports.EnvironmentService }

func (s *environmentService) CreateEnvironment(ctx context.Context, req *connect.Request[pb.CreateEnvironmentRequest]) (*connect.Response[pb.CreateEnvironmentResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	e, err := s.envs.CreateEnvironment(ctx, caller(ctx), repo, req.Msg.GetName())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.CreateEnvironmentResponse{Environment: environmentToPB(e)}), nil
}

func (s *environmentService) ListEnvironments(ctx context.Context, req *connect.Request[pb.ListEnvironmentsRequest]) (*connect.Response[pb.ListEnvironmentsResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	page, err := s.envs.ListEnvironments(ctx, caller(ctx), repo, int(req.Msg.GetPageSize()), req.Msg.GetPageToken())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	out := &pb.ListEnvironmentsResponse{NextPageToken: page.NextPageToken, Environments: make([]*pb.Environment, 0, len(page.Environments))}
	for _, x := range page.Environments {
		out.Environments = append(out.Environments, environmentToPB(x))
	}
	return connect.NewResponse(out), nil
}

func (s *environmentService) DeleteEnvironment(ctx context.Context, req *connect.Request[pb.DeleteEnvironmentRequest]) (*connect.Response[pb.DeleteEnvironmentResponse], error) {
	repo, err := repoOf(req.Msg.GetRepo())
	if err != nil {
		return nil, connectError(ctx, err)
	}
	if err := s.envs.DeleteEnvironment(ctx, caller(ctx), repo, req.Msg.GetName()); err != nil {
		return nil, connectError(ctx, err)
	}
	return connect.NewResponse(&pb.DeleteEnvironmentResponse{}), nil
}
