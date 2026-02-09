# Project Structure and Code Examples for Java Spring Boot Development

## Project Directory Layout
- **src/**: Contains all application source code.
  - **main/**: Application’s main codebase.
    - **java/**: Java source files.
      - **com/example/**: Base package of the application; adjust package structure as needed.
        - **entities/**: Contains entity classes that represent the domain model.
        - **repositories/**: Interfaces for data access (Spring Data JPA repositories).
        - **services/**: Business logic and service implementations.
        - **controllers/**: REST controllers for handling web requests.
        - **dto/**: Data Transfer Objects for transferring data between layers.
        - **exceptions/**: Global exception handlers.
    - **resources/**: Contains application configuration files (e.g., application.properties).

## Entity Examples
```java
package com.example.entities;

import javax.persistence.*;

@Entity
@Table(name = "users")
public class User {
    @Id
    @GeneratedValue(strategy = GenerationType.IDENTITY)
    private Long id;

    private String username;
    private String email;

    // Getters and Setters
}
```

## Repository Interfaces
```java
package com.example.repositories;

import org.springframework.data.jpa.repository.JpaRepository;
import com.example.entities.User;

public interface UserRepository extends JpaRepository<User, Long> {
    Optional<User> findByUsername(String username);
}
```

## Service Implementations
```java
package com.example.services;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import com.example.repositories.UserRepository;
import com.example.entities.User;

@Service
public class UserService {
    @Autowired
    private UserRepository userRepository;

    public User findUserByUsername(String username) {
        return userRepository.findByUsername(username).orElse(null);
    }
}
```

## REST Controllers
```java
package com.example.controllers;

import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;
import com.example.services.UserService;
import com.example.entities.User;

@RestController
@RequestMapping("/api/users")
public class UserController {
    @Autowired
    private UserService userService;

    @GetMapping("/{username}")
    public ResponseEntity<User> getUser(@PathVariable String username) {
        User user = userService.findUserByUsername(username);
        return user != null ? ResponseEntity.ok(user) : ResponseEntity.notFound().build();
    }
}
```

## DTO Classes
```java
package com.example.dto;

public class UserDTO {
    private String username;
    private String email;

    // Getters and Setters
}
```

## Global Exception Handlers
```java
package com.example.exceptions;

import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.ControllerAdvice;
import org.springframework.web.bind.annotation.ExceptionHandler;

@ControllerAdvice
public class GlobalExceptionHandler {

    @ExceptionHandler(Exception.class)
    public ResponseEntity<String> handleException(Exception e) {
        return new ResponseEntity<>(e.getMessage(), HttpStatus.INTERNAL_SERVER_ERROR);
    }
}
```
