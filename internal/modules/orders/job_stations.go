package orders

import (
	"context"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
)

// EnsureProfileStations creates a job profile's default production stations for an outlet that
// has no active station yet. An outlet that already has stations keeps them untouched. Used when
// an admin picks a service profile and by the demo seed, so both set up the same board.
func EnsureProfileStations(ctx context.Context, client *ent.Client, tenantID, outletID uuid.UUID, profile outletpolicy.ServiceProfile) error {
	if len(profile.DefaultStations) == 0 {
		return nil
	}
	exists, err := client.KDSStation.Query().
		Where(kdsstation.TenantID(tenantID), kdsstation.OutletID(outletID), kdsstation.IsActive(true)).
		Exist(ctx)
	if err != nil || exists {
		return err
	}
	builders := make([]*ent.KDSStationCreate, 0, len(profile.DefaultStations))
	for i, st := range profile.DefaultStations {
		builders = append(builders, client.KDSStation.Create().
			SetTenantID(tenantID).
			SetOutletID(outletID).
			SetName(st.Name).
			SetStationType(kdsstation.StationType(st.StationType)).
			SetCategoryFilter(st.CategoryFilter).
			SetSortOrder(i))
	}
	_, err = client.KDSStation.CreateBulk(builders...).Save(ctx)
	return err
}
