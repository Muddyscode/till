package httpserver

import (
	"github.com/Muddyscode/till/internal/auth"
	"github.com/go-chi/chi/v5"
)

// registerOperations mounts the Phase 3B routes inside the authenticated group.
func (s *Server) registerOperations(r chi.Router) {
	// Any signed-in staff: till sessions.
	r.Post("/v1/shifts/open", s.openShift)
	r.Get("/v1/shifts", s.listShifts)
	r.Get("/v1/shifts/{id}", s.getShift)
	r.Get("/v1/shifts/{id}/z-report", s.shiftZReport)
	r.Post("/v1/shifts/{id}/close", s.closeShift)

	r.Group(func(r chi.Router) {
		r.Use(s.requireRole(auth.RoleAdmin, auth.RoleManager))
		r.Post("/v1/stock/transfers", s.createTransfer)
		r.Get("/v1/stock/transfers", s.listStockTransfers)
		r.Get("/v1/stock/transfers/{id}", s.getTransfer)
		r.Post("/v1/stock/transfers/{id}/receive", s.receiveTransfer)
		r.Post("/v1/stock/transfers/{id}/cancel", s.cancelTransfer)
		r.Post("/v1/stock/counts", s.createStockCount)
		r.Get("/v1/stock/counts/{id}", s.getStockCount)
		r.Post("/v1/suppliers", s.createSupplier)
		r.Get("/v1/suppliers", s.listSuppliers)
		r.Get("/v1/suppliers/{id}", s.getSupplier)
		r.Patch("/v1/suppliers/{id}", s.patchSupplier)
		r.Delete("/v1/suppliers/{id}", s.deleteSupplier)
		r.Get("/v1/suppliers/{id}/statement", s.supplierStatement)
		r.Post("/v1/supplier-payments", s.createSupplierPayment)
	})

	r.Group(func(r chi.Router) {
		r.Use(s.requireRole(auth.RoleAdmin))
		r.Get("/v1/events", s.listEvents)
		r.Post("/v1/webhook-endpoints", s.createWebhookEndpoint)
		r.Get("/v1/webhook-endpoints", s.listWebhookEndpoints)
		r.Delete("/v1/webhook-endpoints/{id}", s.deleteWebhookEndpoint)
		r.Get("/v1/webhook-endpoints/{id}/deliveries", s.listWebhookDeliveries)
		r.Get("/v1/reports/vat", s.vatReport)
	})
}
