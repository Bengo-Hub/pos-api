-- Reverse staff↔employee sync: POS StaffMember carries the ERP HR employee number (projected from
-- erp.employee.upserted) so staff funded from salary show their payroll number in POS.
ALTER TABLE "staff_members" ADD COLUMN "erp_employee_number" character varying NULL;
