// Package multiproc holds the integration scenarios of step 14: they run
// real server processes (HTTP and the background auction loop of each) as
// separate OS processes against a real PostgreSQL, and drive them through
// HTTP with browser-like cookie and CSRF handling. The scenarios cover
// multi-process competition, replacement and crash of replicas, and the
// unavailability and recovery of the database.
//
// The package deliberately carries no application code: every file except
// doc.go is a //go:build integration test, and make test runs it as
// "no test files".
package multiproc
