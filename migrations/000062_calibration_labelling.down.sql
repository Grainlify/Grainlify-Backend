DROP TRIGGER IF EXISTS trg_calibration_labels_append_only ON calibration_labels;
DROP FUNCTION IF EXISTS calibration_labels_are_append_only();
DROP TABLE IF EXISTS calibration_labels;
DROP TABLE IF EXISTS calibration_pr_snapshots;
DROP TABLE IF EXISTS calibration_sample_prs;
DROP TABLE IF EXISTS calibration_samples;
