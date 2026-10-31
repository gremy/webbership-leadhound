package console

import (
	"context"
)

// sendNowFake is the manual Send now part of fakeBackend: canned answers, and what each call asked.
type sendNowFake struct {
	plan        SendPlan // returned by SendNowPreview and SendNowPlan (ErrNotFound for another batch id)
	previewErr  error    // returned by SendNowPreview
	planErr     error    // returned by SendNowPlan
	confirmOut  ControlOutcome
	confirmErr  error // returned by SendNowConfirm
	checkOut    ControlOutcome
	checkErr    error // returned by SendNowCheckEmail
	lastPreview ManualSendRequest
	lastConfirm ManualConfirm
	lastCheck   int64
}

func (f *fakeBackend) SendNowPreview(_ context.Context, req ManualSendRequest) (SendPlan, error) {
	f.count("SendNowPreview")
	f.mu.Lock()
	f.lastPreview = req
	f.mu.Unlock()
	if f.previewErr != nil {
		return SendPlan{}, f.previewErr
	}
	return f.plan, nil
}

func (f *fakeBackend) SendNowPlan(_ context.Context, id int64) (SendPlan, error) {
	f.count("SendNowPlan")
	if f.planErr != nil {
		return SendPlan{}, f.planErr
	}
	if err := f.leadsRead(); err != nil {
		return SendPlan{}, err
	}
	if id != f.plan.BatchID {
		return SendPlan{}, ErrNotFound
	}
	return f.plan, nil
}

func (f *fakeBackend) SendNowConfirm(_ context.Context, c ManualConfirm) (ControlOutcome, error) {
	f.count("SendNowConfirm")
	f.mu.Lock()
	f.lastConfirm = c
	f.mu.Unlock()
	return f.confirmOut, f.confirmErr
}

func (f *fakeBackend) SendNowCheckEmail(_ context.Context, id int64) (ControlOutcome, error) {
	f.count("SendNowCheckEmail")
	f.mu.Lock()
	f.lastCheck = id
	f.mu.Unlock()
	return f.checkOut, f.checkErr
}

// richSendPlan is manual batch 7 (the routes' sample) with one lead of each kind and every warning.
func richSendPlan(text string) SendPlan {
	p := SendPlan{
		BatchID: 7, HashPrefix: "fedcba987654",
		Leads: []SendPlanLead{
			{LeadID: 5, Name: text, Domain: "brightwater-dental.example", Email: "lena@brightwater-dental.example", OK: true},
			{LeadID: 6, Name: "Corner Bean Cafe", Domain: "cornerbean.test", Email: "hi@cornerbean.test", OK: true},
			{LeadID: 8, Name: "Fernway Clinic", Domain: "fernway.example", Email: "info@fernway.example", Stop: "The email check says invalid: the mailbox does not exist."},
			{LeadID: 9, Name: "Pine Hollow Bakery", Domain: "pinehollow.test", Email: "orders@pinehollow.test", Waiting: "email not checked yet"},
		},
		Notes:   []string{"One email per recipient domain goes in each run. Leads sharing a domain go in the next run, a few minutes later."},
		CanSend: true,
	}
	for _, c := range SendWarningCodes {
		p.Warnings = append(p.Warnings, SendWarning{Code: c, Sentence: "Warning " + c + ": 18 of 18 today. This goes 1 over."})
	}
	return p
}
