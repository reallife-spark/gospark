package delegates

import (
	"errors"
	"fmt"
	"regexp"
)

const skillEvalSchema = "sparknet-skill-eval/1"

var (
	skillEvalVerdicts     = map[string]bool{"pass": true, "warn": true, "fail": true}
	skillEvalSubjectKinds = map[string]bool{
		"delegate-run": true, "self-improve-candidate": true, "plan": true, "rfi-answer": true, "skill": true,
	}
	skillEvalEvaluatorKind = map[string]bool{"agent": true, "harness": true, "human": true}
	skillEvalForbidden     = map[string]bool{
		"capability": true, "grant": true, "signature": true, "approve": true, "application": true,
		"apply": true, "secret": true, "token": true, "key": true,
	}
	skillEvalDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidateSkillEval refuses a malformed or authority-shaped evaluation record.
// The canonical digest is produced and verified by shared/sparknet-skill-eval.py;
// the broker validates the closed shape on the way in and stores the record as
// evidence. A nil record means the reviewer returned nothing and is allowed
// here: whether that blocks a promotion is the consumer's decision.
func ValidateSkillEval(record map[string]any) error {
	if record == nil {
		return nil
	}
	if err := checkSkillEvalForbidden(record, "record"); err != nil {
		return err
	}
	if record["schema"] != skillEvalSchema {
		return fmt.Errorf("skill eval: schema must be %s", skillEvalSchema)
	}
	verdict, _ := record["verdict"].(string)
	if !skillEvalVerdicts[verdict] {
		return fmt.Errorf("skill eval: unknown verdict %q", verdict)
	}
	digest, _ := record["body_sha256"].(string)
	if !skillEvalDigest.MatchString(digest) {
		return errors.New("skill eval: body_sha256 must be a lowercase sha256 digest")
	}
	subject, ok := record["subject"].(map[string]any)
	if !ok {
		return errors.New("skill eval: subject must be an object")
	}
	subjectKind, _ := subject["kind"].(string)
	if !skillEvalSubjectKinds[subjectKind] {
		return fmt.Errorf("skill eval: unknown subject kind %q", subjectKind)
	}
	if ref, _ := subject["ref"].(string); ref == "" {
		return errors.New("skill eval: subject ref is required")
	}
	evaluator, ok := record["evaluator"].(map[string]any)
	if !ok {
		return errors.New("skill eval: evaluator must be an object")
	}
	evaluatorKind, _ := evaluator["kind"].(string)
	if !skillEvalEvaluatorKind[evaluatorKind] {
		return fmt.Errorf("skill eval: unknown evaluator kind %q", evaluatorKind)
	}
	return nil
}

func checkSkillEvalForbidden(value any, path string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if skillEvalForbidden[key] {
				return fmt.Errorf("skill eval: forbidden authority field %q", path+"."+key)
			}
			if err := checkSkillEvalForbidden(child, path+"."+key); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := checkSkillEvalForbidden(child, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}
