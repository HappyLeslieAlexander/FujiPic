package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/bucket"
	"fujipic/internal/id"
	"fujipic/internal/imagefile"
	"fujipic/internal/model"
	"fujipic/internal/store"
	"fujipic/internal/upload"
)

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if webUserFrom(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if webUserFrom(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", pageData{Title: "登录", CSRF: s.ensureCSRF(w, r)})
}

// normalizeEmail trims, lowercases and extracts the bare address from inputs
// like "Alice <alice@example.com>". The returned string is the only form
// stored or looked up, so register/login stay consistent regardless of which
// form the user typed.
func normalizeEmail(raw string) string {
	s := strings.TrimSpace(raw)
	if addr, err := mail.ParseAddress(s); err == nil && addr.Address != "" {
		s = addr.Address
	}
	return strings.ToLower(strings.TrimSpace(s))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败，请刷新页面重试", http.StatusForbidden)
		return
	}
	email := normalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")
	ip := s.clientIP(r)

	if !s.limiter.allow("login:ip:"+ip, 10, time.Minute) || !s.limiter.allow("login:email:"+email, 5, time.Minute) {
		s.render(w, r, "login", pageData{Title: "登录", Email: email, Error: "尝试次数过多，请稍后再试。"})
		return
	}
	if email == "" || password == "" {
		s.render(w, r, "login", pageData{Title: "登录", Email: email, Error: "请输入邮箱和密码。"})
		return
	}

	user, err := s.store.UserByEmail(r.Context(), email)
	if err != nil {
		auth.DummyVerify(password)
		s.render(w, r, "login", pageData{Title: "登录", Email: email, Error: "邮箱或密码不正确。"})
		return
	}
	ok, err := auth.VerifyPassword(password, user.PasswordHash)
	if err != nil || !ok {
		s.render(w, r, "login", pageData{Title: "登录", Email: email, Error: "邮箱或密码不正确。"})
		return
	}
	if !user.IsActive() {
		s.render(w, r, "login", pageData{Title: "登录", Email: email, Error: "账号已被禁用，请联系管理员。"})
		return
	}
	if err := s.createSession(r.Context(), w, r, user.ID); err != nil {
		s.log.Error("create session", "err", err)
		http.Error(w, "登录失败，请稍后重试", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleRegisterPage(w http.ResponseWriter, r *http.Request) {
	if webUserFrom(r) != nil {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	s.render(w, r, "register", pageData{
		Title:            "注册",
		CSRF:             s.ensureCSRF(w, r),
		OpenRegistration: s.cfg.AllowOpenRegistration,
	})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败，请刷新页面重试", http.StatusForbidden)
		return
	}
	email := normalizeEmail(r.FormValue("email"))
	password := r.FormValue("password")
	invite := strings.TrimSpace(r.FormValue("invite"))
	ip := s.clientIP(r)

	renderErr := func(msg string) {
		s.render(w, r, "register", pageData{
			Title:            "注册",
			Email:            email,
			Invite:           invite,
			OpenRegistration: s.cfg.AllowOpenRegistration,
			Error:            msg,
		})
	}

	if !s.limiter.allow("register:ip:"+ip, 5, time.Hour) {
		renderErr("注册请求过于频繁，请稍后再试。")
		return
	}
	if _, err := mail.ParseAddress(email); err != nil || !strings.Contains(email, "@") || len(email) > 254 {
		renderErr("请输入有效的邮箱地址。")
		return
	}
	if len(password) < 10 || len(password) > 200 {
		renderErr("密码长度需在 10 到 200 个字符之间。")
		return
	}
	if !s.cfg.AllowOpenRegistration && invite == "" {
		renderErr("需要邀请码才能注册。")
		return
	}
	if invite == "" {
		invite = ""
	}
	if _, err := s.store.UserByEmail(r.Context(), email); err == nil {
		renderErr("该邮箱已注册。")
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("check registration email", "err", err)
		http.Error(w, "注册失败", http.StatusInternalServerError)
		return
	}
	if invite != "" {
		if err := s.store.ValidateInvite(r.Context(), invite); errors.Is(err, store.ErrInviteInvalid) {
			renderErr("邀请码无效、已使用或已过期。")
			return
		} else if err != nil {
			s.log.Error("validate invite", "err", err)
			http.Error(w, "注册失败", http.StatusInternalServerError)
			return
		}
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		s.log.Error("hash password", "err", err)
		http.Error(w, "注册失败", http.StatusInternalServerError)
		return
	}
	now := time.Now().Unix()
	user := &model.User{
		ID:           id.New("usr"),
		Email:        email,
		PasswordHash: hash,
		Role:         "user",
		Status:       "active",
		QuotaBytes:   s.cfg.DefaultQuotaBytes,
		UsedBytes:    0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.store.CreateUserWithInvite(r.Context(), user, invite); err != nil {
		switch {
		case errors.Is(err, store.ErrInviteInvalid):
			renderErr("邀请码无效、已使用或已过期。")
		case errors.Is(err, store.ErrConflict):
			renderErr("该邮箱已注册。")
		default:
			s.log.Error("create user", "err", err)
			http.Error(w, "注册失败", http.StatusInternalServerError)
		}
		return
	}
	s.audit(r.Context(), user.ID, "user.register", user.ID, ip, "")
	if err := s.createSession(r.Context(), w, r, user.ID); err != nil {
		s.log.Error("create session after register", "err", err)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	s.destroySession(r.Context(), w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user := s.requireWebUser(w, r)
	if user == nil {
		return
	}
	images, err := s.store.ListImages(r.Context(), user.ID, 60, 0, "")
	if err != nil {
		s.log.Error("list images", "err", err)
		http.Error(w, "加载图库失败", http.StatusInternalServerError)
		return
	}
	count, _ := s.store.CountUserImages(r.Context(), user.ID)
	views := make([]imageView, 0, len(images))
	for i := range images {
		views = append(views, imageView{
			Image: images[i],
			URL:   s.uploads.URL(&images[i]),
			Name:  displayName(images[i].OriginalName),
		})
	}
	s.render(w, r, "dashboard", pageData{
		Title:        "图库",
		Images:       views,
		ImageCount:   count,
		QuotaPercent: quotaPercent(user.UsedBytes, user.QuotaBytes),
	})
}

func (s *Server) handleWebUpload(w http.ResponseWriter, r *http.Request) {
	user := s.requireWebUser(w, r)
	if user == nil {
		return
	}
	// Share the API upload bucket so web and API callers cannot bypass each
	// other's rate limit by switching surface.
	if !s.limiter.allow("upload:"+user.ID, 60, time.Minute) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "请求过于频繁，请稍后再试", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		s.setFlash(w, "上传失败：请求体过大或格式不正确。")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败，请刷新页面重试", http.StatusForbidden)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		s.setFlash(w, "上传失败：没有找到文件字段。")
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	defer file.Close()
	name := header.Filename
	if v := r.FormValue("name"); v != "" {
		name = v
	}
	res, err := s.uploads.Upload(r.Context(), user, file, name, "public")
	if err != nil {
		s.setFlash(w, "上传失败："+uploadErrorMessage(err))
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}
	if res.Deduplicated {
		s.setFlash(w, "该图片已存在，返回原链接。")
	} else {
		s.setFlash(w, "上传成功。")
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	user := s.requireWebUser(w, r)
	if user == nil {
		return
	}
	s.renderSettings(w, r, user, pageData{})
}

// renderSettings loads the settings page's shared token data so password and
// token errors can re-render the full page.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, user *model.User, data pageData) {
	tokens, err := s.store.ListTokens(r.Context(), user.ID)
	if err != nil {
		s.log.Error("list tokens", "err", err)
		http.Error(w, "加载设置失败", http.StatusInternalServerError)
		return
	}
	data.Title = "设置"
	data.Tokens = tokens
	s.render(w, r, "settings", data)
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	user := s.requireWebUser(w, r)
	if user == nil {
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	current := r.FormValue("current")
	newPassword := r.FormValue("new")
	confirm := r.FormValue("confirm")
	info := authFrom(r)

	renderErr := func(msg string) {
		s.renderSettings(w, r, user, pageData{Error: msg})
	}
	// PLAN §5.6: at most 5 password-change attempts per hour per user. The
	// check runs before password verification so guessing the current
	// password is throttled too.
	if !s.limiter.allow("password:"+user.ID, 5, time.Hour) {
		renderErr("尝试次数过多，请稍后再试。")
		return
	}
	if ok, err := auth.VerifyPassword(current, user.PasswordHash); err != nil || !ok {
		renderErr("当前密码不正确。")
		return
	}
	if len(newPassword) < 10 || len(newPassword) > 200 {
		renderErr("新密码长度需在 10 到 200 个字符之间。")
		return
	}
	if newPassword != confirm {
		renderErr("两次输入的新密码不一致。")
		return
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		http.Error(w, "修改密码失败", http.StatusInternalServerError)
		return
	}
	keepSessionID := ""
	if info != nil {
		keepSessionID = info.SessionID
	}
	if err := s.store.UpdatePasswordAndRevokeCredentials(r.Context(), user.ID, hash, keepSessionID); err != nil {
		http.Error(w, "修改密码失败", http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), user.ID, "user.password_change", user.ID, s.clientIP(r), "")
	s.setFlash(w, "密码已更新，其他设备已退出登录。")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) handleTokenCreateWeb(w http.ResponseWriter, r *http.Request) {
	user := s.requireWebUser(w, r)
	if user == nil {
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "未命名"
	}
	name = truncate(name, 80)
	scopes, ok := auth.NormalizeScopes(r.FormValue("scopes"))
	if !ok {
		s.renderSettings(w, r, user, pageData{Error: "权限范围不合法。"})
		return
	}
	var expires *int64
	if daysStr := strings.TrimSpace(r.FormValue("days")); daysStr != "" {
		days, err := strconv.Atoi(daysStr)
		if err != nil || days < 1 || days > 3650 {
			s.renderSettings(w, r, user, pageData{Error: "有效天数需在 1 到 3650 之间。"})
			return
		}
		v := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
		expires = &v
	}
	plaintext, hash, prefix := auth.NewToken()
	tok := &model.APIToken{
		ID:          id.New("tok"),
		UserID:      user.ID,
		Name:        name,
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scopes:      scopes,
		CreatedAt:   time.Now().Unix(),
		ExpiresAt:   expires,
	}
	if err := s.store.CreateToken(r.Context(), tok); err != nil {
		if errors.Is(err, store.ErrTokenLimitExceeded) {
			s.renderSettings(w, r, user, pageData{Error: "Token 数量已达到上限，请先撤销旧 Token。"})
			return
		}
		s.log.Error("create token", "err", err)
		http.Error(w, "创建 Token 失败", http.StatusInternalServerError)
		return
	}
	s.audit(r.Context(), user.ID, "token.create", tok.ID, s.clientIP(r), name)
	s.renderSettings(w, r, user, pageData{NewToken: plaintext})
}

func (s *Server) handleTokenDeleteWeb(w http.ResponseWriter, r *http.Request) {
	user := s.requireWebUser(w, r)
	if user == nil {
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	tokenID := r.PathValue("id")
	if err := s.store.DeleteToken(r.Context(), user.ID, tokenID); err != nil {
		s.setFlash(w, "Token 不存在或已撤销。")
	} else {
		s.audit(r.Context(), user.ID, "token.revoke", tokenID, s.clientIP(r), "")
		s.setFlash(w, "Token 已撤销。")
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) handleAdminBucketCreate(w http.ResponseWriter, r *http.Request) {
	admin := s.requireWebUser(w, r)
	if admin == nil {
		return
	}
	if !admin.IsAdmin() {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	if !s.limiter.allow("bucket-admin:"+admin.ID, 30, time.Hour) {
		s.setFlash(w, "添加存储桶过于频繁，请稍后再试。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	maxBytes, err := parseCapacityGiB(r.FormValue("max_gb"))
	if err != nil {
		s.setFlash(w, "最大容量格式不正确。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	priority, err := parsePriority(r.FormValue("priority"))
	if err != nil {
		s.setFlash(w, "优先级需在 0 到 1000000 之间。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	b, err := s.buckets.Add(r.Context(), bucket.AddInput{
		Name:            r.FormValue("name"),
		Endpoint:        r.FormValue("endpoint"),
		Region:          r.FormValue("region"),
		Bucket:          r.FormValue("bucket"),
		AccessKeyID:     r.FormValue("access_key_id"),
		SecretAccessKey: r.FormValue("secret_access_key"),
		ForcePathStyle:  r.FormValue("force_path_style") != "",
		MaxBytes:        maxBytes,
		Priority:        priority,
	})
	if err != nil {
		s.setFlash(w, bucketErrorMessage(err))
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.audit(r.Context(), admin.ID, "bucket.create", b.ID, s.clientIP(r), b.Name)
	s.setFlash(w, "存储桶已添加并通过读写检查。")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminBucketUpdate(w http.ResponseWriter, r *http.Request) {
	admin := s.requireWebUser(w, r)
	if admin == nil {
		return
	}
	if !admin.IsAdmin() {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	bucketID := r.PathValue("id")
	maxBytes, err := parseCapacityGiB(r.FormValue("max_gb"))
	if err != nil {
		s.setFlash(w, "最大容量格式不正确。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	priority, err := parsePriority(r.FormValue("priority"))
	if err != nil {
		s.setFlash(w, "优先级需在 0 到 1000000 之间。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	err = s.buckets.Update(r.Context(), bucketID, bucket.UpdateInput{
		Name:     r.FormValue("name"),
		MaxBytes: maxBytes,
		Priority: priority,
		Enabled:  r.FormValue("enabled") != "",
	})
	if err != nil {
		s.setFlash(w, bucketErrorMessage(err))
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.audit(r.Context(), admin.ID, "bucket.update", bucketID, s.clientIP(r), "")
	s.setFlash(w, "存储桶设置已更新。")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminBucketDelete(w http.ResponseWriter, r *http.Request) {
	admin := s.requireWebUser(w, r)
	if admin == nil {
		return
	}
	if !admin.IsAdmin() {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	bucketID := r.PathValue("id")
	if err := s.buckets.Delete(r.Context(), bucketID); err != nil {
		switch {
		case errors.Is(err, bucket.ErrInUse):
			s.setFlash(w, "该存储桶仍有图片、待重试的删除任务或正在进行的上传，不能删除。")
		case errors.Is(err, bucket.ErrDefaultBucket):
			s.setFlash(w, "服务器默认存储桶不能删除。")
		case errors.Is(err, bucket.ErrNotFound):
			s.setFlash(w, "存储桶不存在。")
		default:
			s.log.Error("delete storage bucket", "err", err)
			s.setFlash(w, "删除存储桶失败。")
		}
	} else {
		s.audit(r.Context(), admin.ID, "bucket.delete", bucketID, s.clientIP(r), "")
		s.setFlash(w, "存储桶已删除。")
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func parseCapacityGiB(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	gb, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(gb) || gb < 0 || gb > 1e9 {
		return 0, errors.New("invalid capacity")
	}
	bytes := int64(gb * float64(int64(1)<<30))
	// 0 means unlimited, so a tiny positive capacity must not round down
	// to unlimited.
	if gb > 0 && bytes == 0 {
		bytes = 1
	}
	return bytes, nil
}

func parsePriority(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 100, nil
	}
	priority, err := strconv.Atoi(raw)
	if err != nil || priority < 0 || priority > 1_000_000 {
		return 0, errors.New("invalid priority")
	}
	return priority, nil
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	user := s.requireWebUser(w, r)
	if user == nil {
		return
	}
	if !user.IsAdmin() {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}
	s.renderAdmin(w, r, "")
}

func (s *Server) renderAdmin(w http.ResponseWriter, r *http.Request, newInvite string) {
	user := webUserFrom(r)
	stats, err := s.store.Stats(r.Context())
	if err != nil {
		s.log.Error("load stats", "err", err)
		http.Error(w, "加载管理数据失败", http.StatusInternalServerError)
		return
	}
	users, err := s.store.ListUsers(r.Context(), 100)
	if err != nil {
		http.Error(w, "加载用户失败", http.StatusInternalServerError)
		return
	}
	invites, _ := s.store.ListInvites(r.Context(), 50)
	audit, _ := s.store.ListAudit(r.Context(), 50)
	buckets, err := s.buckets.List(r.Context())
	if err != nil {
		s.log.Error("list storage buckets", "err", err)
		http.Error(w, "加载存储桶失败", http.StatusInternalServerError)
		return
	}
	s.render(w, r, "admin", pageData{
		Title:            "管理",
		User:             user,
		Stats:            stats,
		Users:            users,
		Invites:          invites,
		Audit:            audit,
		NewInvite:        newInvite,
		Buckets:          buckets,
		BucketEncryption: s.buckets.Enabled(),
	})
}

func (s *Server) handleAdminUserStatus(w http.ResponseWriter, r *http.Request) {
	admin := s.requireWebUser(w, r)
	if admin == nil {
		return
	}
	if !admin.IsAdmin() {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	userID := r.PathValue("id")
	status := r.FormValue("status")
	if status != "active" && status != "disabled" {
		s.setFlash(w, "状态值不合法。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	if err := s.store.UpdateUserStatus(r.Context(), userID, status); err != nil {
		if errors.Is(err, store.ErrLastAdmin) {
			s.setFlash(w, "不能禁用最后一个管理员。")
		} else {
			s.setFlash(w, "更新用户状态失败。")
		}
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.audit(r.Context(), admin.ID, "admin.user_status", userID, s.clientIP(r), status)
	s.setFlash(w, "用户状态已更新。")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminUserQuota(w http.ResponseWriter, r *http.Request) {
	admin := s.requireWebUser(w, r)
	if admin == nil {
		return
	}
	if !admin.IsAdmin() {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	userID := r.PathValue("id")
	mb, err := strconv.ParseInt(r.FormValue("quota_mb"), 10, 64)
	if err != nil || mb < 1 || mb > 1<<20 {
		s.setFlash(w, "配额需在 1 到 1048576 MiB 之间。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	quota := mb << 20
	if err := s.store.UpdateUserQuota(r.Context(), userID, quota); err != nil {
		s.setFlash(w, "更新配额失败。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.audit(r.Context(), admin.ID, "admin.user_quota", userID, s.clientIP(r), strconv.FormatInt(quota, 10))
	s.setFlash(w, "配额已更新。")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminInvite(w http.ResponseWriter, r *http.Request) {
	admin := s.requireWebUser(w, r)
	if admin == nil {
		return
	}
	if !admin.IsAdmin() {
		http.Error(w, "需要管理员权限", http.StatusForbidden)
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	var expires *int64
	if daysStr := strings.TrimSpace(r.FormValue("days")); daysStr != "" {
		days, err := strconv.Atoi(daysStr)
		if err != nil || days < 1 || days > 3650 {
			s.setFlash(w, "有效天数需在 1 到 3650 之间。")
			http.Redirect(w, r, "/admin", http.StatusSeeOther)
			return
		}
		v := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
		expires = &v
	}
	code := id.Random(9)
	inv := &model.Invite{
		Code:      code,
		CreatedBy: &admin.ID,
		CreatedAt: time.Now().Unix(),
		ExpiresAt: expires,
	}
	if err := s.store.CreateInvite(r.Context(), inv); err != nil {
		s.log.Error("create invite", "err", err)
		s.setFlash(w, "创建邀请码失败。")
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	s.audit(r.Context(), admin.ID, "admin.invite_create", code, s.clientIP(r), "")
	s.renderAdmin(w, r, code)
}

func (s *Server) audit(ctx context.Context, actorID, action, target, ip, meta string) {
	err := s.store.AddAudit(ctx, &model.AuditLog{
		ID:        id.New("aud"),
		ActorID:   &actorID,
		Action:    action,
		Target:    target,
		IP:        ip,
		Meta:      meta,
		CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		s.log.Error("write audit log", "err", err)
	}
}

// displayName canonicalises user-controlled filenames for safe rendering in
// HTML (html/template auto-escaping covers markup, and htmlFor escapes
// explicitly), Markdown embeds, JSON and the gallery. Control characters are
// dropped so a crafted name cannot break out of an alt attribute or a
// Markdown link/label onto the next line.
func displayName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if strings.TrimSpace(cleaned) == "" {
		return "image"
	}
	return cleaned
}

func quotaPercent(used, quota int64) int {
	if quota <= 0 {
		return 0
	}
	p := int(used * 100 / quota)
	if p > 100 {
		return 100
	}
	return p
}

func uploadErrorMessage(err error) string {
	switch {
	case errors.Is(err, imagefile.ErrTooLarge):
		return "图片超过大小限制。"
	case errors.Is(err, imagefile.ErrUnsupported):
		return "不支持的文件类型，只允许 JPEG / PNG / GIF / WebP。"
	case errors.Is(err, imagefile.ErrDimensions):
		return "图片像素或边长超过限制。"
	case errors.Is(err, store.ErrQuotaExceeded):
		return "存储配额不足。"
	case errors.Is(err, upload.ErrNoCapacity):
		return "所有存储桶容量已满，请联系管理员。"
	case errors.Is(err, upload.ErrOnlyPublic):
		return "v1 只支持公开图片。"
	default:
		return "服务器暂时无法保存图片。"
	}
}

func bucketErrorMessage(err error) string {
	switch {
	case errors.Is(err, bucket.ErrNoEncryptionKey):
		return "管理员需要先设置 STORAGE_ENCRYPTION_KEY，才能添加自定义存储桶。"
	case errors.Is(err, bucket.ErrInUse):
		return "该存储桶仍有图片、待重试的删除任务或正在进行的上传，不能删除。"
	case errors.Is(err, bucket.ErrDefaultBucket):
		return "服务器默认存储桶不能删除。"
	case errors.Is(err, bucket.ErrNotFound):
		return "存储桶不存在。"
	case errors.Is(err, bucket.ErrInvalid):
		return strings.TrimPrefix(err.Error(), bucket.ErrInvalid.Error()+": ")
	default:
		return "存储桶操作失败。"
	}
}
