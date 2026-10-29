// SPDX-License-Identifier: AGPL-3.0-only

package imageupdate

// putPlanForTest overwrites a stored plan (tests only).
func (s *Service) putPlanForTest(p Plan) { _ = s.putAny(p) }
