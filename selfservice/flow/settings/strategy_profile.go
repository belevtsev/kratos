package settings

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"
	"github.com/tidwall/sjson"

	"github.com/ory/herodot"
	"github.com/ory/jsonschema/v3"
	"github.com/ory/x/decoderx"
	"github.com/ory/x/urlx"

	"github.com/ory/kratos/continuity"
	"github.com/ory/kratos/driver/configuration"
	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/schema"
	"github.com/ory/kratos/selfservice/errorx"
	"github.com/ory/kratos/selfservice/form"
	"github.com/ory/kratos/session"
	"github.com/ory/kratos/x"
)

const (
	StrategyProfile               = "profile"
	PublicSettingsProfilePath     = "/self-service/browser/flows/settings/strategies/profile"
	strategyProfileContinuityName = "settings_profile"
)

var _ Strategy = new(StrategyTraits)

type (
	strategyDependencies interface {
		x.CSRFProvider
		x.CSRFTokenGeneratorProvider
		x.WriterProvider
		x.LoggingProvider

		continuity.ManagementProvider

		session.HandlerProvider
		session.ManagementProvider

		identity.ValidationProvider
		identity.ManagementProvider
		identity.PrivilegedPoolProvider

		errorx.ManagementProvider

		HookExecutorProvider
		ErrorHandlerProvider
		RequestPersistenceProvider
		StrategyProvider
		HooksProvider

		IdentityTraitsSchemas() schema.Schemas
	}
	StrategyTraits struct {
		c configuration.Provider
		d strategyDependencies
	}
)

// swagger:model traitsFormConfig
type TraitsRequestMethod struct {
	*form.HTMLForm
}

func NewStrategyTraits(d strategyDependencies, c configuration.Provider) *StrategyTraits {
	return &StrategyTraits{c: c, d: d}
}

func (s *StrategyTraits) SettingsStrategyID() string {
	return StrategyProfile
}

func (s *StrategyTraits) RegisterSettingsRoutes(public *x.RouterPublic) {
	redirect := session.RedirectOnUnauthenticated(s.c.LoginURL().String())
	public.POST(PublicSettingsProfilePath, s.d.SessionHandler().IsAuthenticated(s.handleSubmit, redirect))
	public.GET(PublicSettingsProfilePath, s.d.SessionHandler().IsAuthenticated(s.handleSubmit, redirect))
}

func (s *StrategyTraits) PopulateSettingsMethod(r *http.Request, ss *session.Session, pr *Request) error {
	traitsSchema, err := s.c.IdentityTraitsSchemas().FindSchemaByID(ss.Identity.TraitsSchemaID)
	if err != nil {
		return err
	}

	// use a schema compiler that disables identifiers
	schemaCompiler := jsonschema.NewCompiler()

	f, err := form.NewHTMLFormFromJSONSchema(urlx.CopyWithQuery(
		urlx.AppendPaths(s.c.SelfPublicURL(), PublicSettingsProfilePath),
		url.Values{"request": {pr.ID.String()}},
	).String(), traitsSchema.URL, "traits", schemaCompiler)
	if err != nil {
		return err
	}

	f.SetValuesFromJSON(json.RawMessage(ss.Identity.Traits), "traits")
	f.SetCSRF(s.d.GenerateCSRFToken(r))

	if err := f.SortFields(traitsSchema.URL, "traits"); err != nil {
		return err
	}

	pr.Methods[s.SettingsStrategyID()] = &RequestMethod{
		Method: s.SettingsStrategyID(),
		Config: &RequestMethodConfig{RequestMethodConfigurator: &TraitsRequestMethod{HTMLForm: f}},
	}
	return nil
}

// swagger:route POST /self-service/browser/flows/settings/strategies/profile public completeSelfServiceBrowserSettingsProfileStrategyFlow
//
// Complete the browser-based settings flow for profile data
//
// This endpoint completes a browser-based settings flow. This is usually achieved by POSTing data to this
// endpoint.
//
// If the provided profile data is valid against the Identity's Traits JSON Schema, the data will be updated and
// the browser redirected to `url.settings_ui` for further steps.
//
// > This endpoint is NOT INTENDED for API clients and only works with browsers (Chrome, Firefox, ...) and HTML Forms.
//
// More information can be found at [ORY Kratos User Settings & Profile Management Documentation](https://www.ory.sh/docs/next/kratos/self-service/flows/user-settings-profile-management).
//
//     Consumes:
//     - application/json
//     - application/x-www-form-urlencoded
//
//     Schemes: http, https
//
//     Responses:
//       302: emptyResponse
//       500: genericError
func (s *StrategyTraits) handleSubmit(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	ss, err := s.d.SessionManager().FetchFromRequest(r.Context(), r)
	if err != nil {
		s.handleSettingsError(w, r, nil, nil, nil, nil, err)
		return
	}

	var p completeSelfServiceBrowserSettingsStrategyProfileFlowPayload
	if _, err := s.d.ContinuityManager().Continue(r.Context(), r,
		strategyProfileContinuityName,
		continuity.WithIdentity(ss.Identity),
		continuity.WithPayload(&p),
	); err == nil {
		s.continueFlow(w, r, ss, &p)
		return
	}

	option, err := s.newSettingsProfileDecoder(ss.Identity)
	if err != nil {
		s.handleSettingsError(w, r, nil, ss, json.RawMessage(ss.Identity.Traits), &p, err)
		return
	}

	p = completeSelfServiceBrowserSettingsStrategyProfileFlowPayload{}
	if err := decoderx.NewHTTP().Decode(r, &p,
		decoderx.HTTPFormDecoder(),
		option,
		decoderx.HTTPDecoderSetValidatePayloads(false),
		decoderx.HTTPDecoderSetIgnoreParseErrorsStrategy(decoderx.ParseErrorIgnore),
	); err != nil {
		s.handleSettingsError(w, r, nil, ss, json.RawMessage(ss.Identity.Traits), &p, err)
		return
	}

	rid := r.URL.Query().Get("request")
	if len(rid) == 0 {
		s.handleSettingsError(w, r, nil, ss, json.RawMessage(ss.Identity.Traits), &p, errors.WithStack(herodot.ErrBadRequest.WithReasonf("The request query parameter is missing.")))
		return
	}

	p.RequestID = rid
	s.continueFlow(w, r, ss, &p)
}

func (s *StrategyTraits) continueFlow(w http.ResponseWriter, r *http.Request, ss *session.Session, p *completeSelfServiceBrowserSettingsStrategyProfileFlowPayload) {
	ar, err := s.d.SettingsRequestPersister().GetSettingsRequest(r.Context(), x.ParseUUID(p.RequestID))
	if err != nil {
		s.handleSettingsError(w, r, nil, ss, json.RawMessage(ss.Identity.Traits), p, err)
		return
	}

	if err := ar.Valid(ss); err != nil {
		s.handleSettingsError(w, r, ar, ss, json.RawMessage(ss.Identity.Traits), p, err)
		return
	}

	if len(p.Traits) == 0 {
		s.handleSettingsError(w, r, ar, ss, json.RawMessage(ss.Identity.Traits), p, errors.WithStack(herodot.ErrBadRequest.WithReasonf("Did not receive any value changes.")))
		return
	}

	if err := s.hydrateForm(r, ar, ss, p.Traits); err != nil {
		s.d.SettingsRequestErrorHandler().HandleSettingsError(w, r, ar, err, StrategyProfile)
		return
	}

	update, err := s.d.PrivilegedIdentityPool().GetIdentityConfidential(context.Background(), ss.Identity.ID)
	if err != nil {
		s.d.SettingsRequestErrorHandler().HandleSettingsError(w, r, ar, err, StrategyProfile)
		return
	}
	update.Traits = identity.Traits(p.Traits)

	if err := s.d.SettingsHookExecutor().PostSettingsHook(w, r,
		StrategyProfile,
		ar, ss, update,
	); err != nil {
		s.handleSettingsError(w, r, ar, ss, p.Traits, p, err)
		return
	}
}

// Complete profile update payload
//
// swagger:parameters completeSelfServiceBrowserSettingsProfileStrategyFlow
// nolint:deadcode,unused
type completeSelfServiceBrowserSettingsProfileStrategyFlow struct {
	// Request is the request ID.
	//
	// required: true
	// in: query
	// format: uuid
	Request string `json:"request"`

	// in: body
	// required: true
	Body completeSelfServiceBrowserSettingsStrategyProfileFlowPayload
}

// swagger:model completeSelfServiceBrowserSettingsStrategyProfileFlowPayload
// nolint:deadcode,unused
type completeSelfServiceBrowserSettingsStrategyProfileFlowPayload struct {
	// Traits contains all of the identity's traits.
	//
	// type: string
	// format: binary
	// required: true
	Traits json.RawMessage `json:"traits"`

	// RequestID is request ID.
	//
	// in: query
	RequestID string `json:"request_id"`
}

func (s *StrategyTraits) hydrateForm(r *http.Request, ar *Request, ss *session.Session, traits json.RawMessage) error {
	action := urlx.CopyWithQuery(
		urlx.AppendPaths(s.c.SelfPublicURL(), PublicSettingsProfilePath),
		url.Values{"request": {ar.ID.String()}},
	)

	ar.Methods[StrategyProfile].Config.Reset()
	if traits != nil {
		for _, field := range form.NewHTMLFormFromJSON(action.String(), traits, "traits").Fields {
			ar.Methods[StrategyProfile].Config.SetField(field)
		}
	}
	ar.Methods[StrategyProfile].Config.SetCSRF(s.d.GenerateCSRFToken(r))

	traitsSchema, err := s.c.IdentityTraitsSchemas().FindSchemaByID(ss.Identity.TraitsSchemaID)
	if err != nil {
		return err
	}

	if err = ar.Methods[StrategyProfile].Config.SortFields(traitsSchema.URL, "traits"); err != nil {
		return err
	}

	return nil
}

// handleSettingsError is a convenience function for handling all types of errors that may occur (e.g. validation error)
// during a settings request.
func (s *StrategyTraits) handleSettingsError(w http.ResponseWriter, r *http.Request, rr *Request, ss *session.Session, traits json.RawMessage, p *completeSelfServiceBrowserSettingsStrategyProfileFlowPayload, err error) {
	if errors.Is(err, ErrRequestNeedsReAuthentication) {
		if err := s.d.ContinuityManager().Pause(
			r.Context(), w, r,
			strategyProfileContinuityName,
			continuity.WithPayload(p),
			continuity.WithIdentity(ss.Identity),
			continuity.WithLifespan(time.Minute*15),
		); err != nil {
			s.d.SettingsRequestErrorHandler().HandleSettingsError(w, r, rr, err, s.SettingsStrategyID())
			return
		}
	}

	if rr != nil {
		if err := s.hydrateForm(r, rr, ss, traits); err != nil {
			s.d.SettingsRequestErrorHandler().HandleSettingsError(w, r, rr, err, s.SettingsStrategyID())
			return
		}
	}

	s.d.SettingsRequestErrorHandler().HandleSettingsError(w, r, rr, err, s.SettingsStrategyID())
}

// newSettingsProfileDecoder returns a decoderx.HTTPDecoderOption with a JSON Schema for type assertion and
// validation.
func (s *StrategyTraits) newSettingsProfileDecoder(i *identity.Identity) (decoderx.HTTPDecoderOption, error) {
	const registrationFormPayloadSchema = `
{
  "$id": "./selfservice/settings/decoder.schema.json",
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "required": ["traits"],
  "properties": {
    "traits": {}
  }
}
`

	ss, err := s.d.IdentityTraitsSchemas().GetByID(i.TraitsSchemaID)
	if err != nil {
		return nil, err
	}
	raw, err := sjson.SetBytes(
		[]byte(registrationFormPayloadSchema),
		"properties.traits.$ref",
		ss.URL.String(),
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	o, err := decoderx.HTTPRawJSONSchemaCompiler(raw)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return o, nil
}
